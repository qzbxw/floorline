package app

import (
	"bufio"
	"context"
	"fmt"
	"io"

	"strings"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"

	"floorline/internal/config"
	"floorline/internal/tgsession"
)

// Login runs the one-time interactive Telegram sign-in.
//
// It is a separate command rather than something the desk attempts on its own
// because Telegram sends a code to the device: there is no unattended path, and
// pretending otherwise would mean a bot that silently stops working whenever
// the session lapses. Run it once; the session then persists across restarts.
func Login(ctx context.Context, cfg *config.Config, in io.Reader, out io.Writer) error {
	client, err := tgsession.New(sessionConfig(cfg))
	if err != nil {
		return err
	}

	phone := strings.TrimSpace(cfg.TelegramPhone)
	reader := bufio.NewReader(in)
	if phone == "" {
		fmt.Fprint(out, "Телефон (в формате +7...): ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		phone = strings.TrimSpace(line)
	}
	if phone == "" {
		return fmt.Errorf("нужен номер телефона")
	}

	err = client.Login(ctx, auth.CodeOnly(phone, auth.CodeAuthenticatorFunc(
		func(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
			fmt.Fprint(out, "Код из Telegram: ")
			line, err := reader.ReadString('\n')
			return strings.TrimSpace(line), err
		})))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nГотово. Сессия лежит в %s — больше вводить код не надо.\n", cfg.SessionPath)
	return nil
}

// sessionConfig maps the environment onto the session client's settings.
func sessionConfig(cfg *config.Config) tgsession.Config {
	return tgsession.Config{
		// The same egress the marketplace uses. MTProto is refused on this
		// server's own address — the port is open and the handshake times out —
		// so the session needs the proxy as much as the trading does.
		Proxy:       cfg.TelegramProxy,
		AppID:       cfg.TelegramAppID,
		AppHash:     cfg.TelegramAppHash,
		Phone:       cfg.TelegramPhone,
		SessionFile: cfg.SessionPath,
	}
}

// sessionAdapter narrows the session client to what the marketplace clients
// need, and maps a venue name onto the mini app that serves it.
type sessionAdapter struct{ c *tgsession.Client }

// Ready reports whether the account has been signed in. A session file is the
// only evidence that `floorline login` ever completed.
func (s sessionAdapter) Ready() bool { return s.c != nil && s.c.LoggedIn() }

func (s sessionAdapter) InitData(ctx context.Context, venue string) (string, error) {
	app, ok := miniApps[venue]
	if !ok {
		return "", fmt.Errorf("нет мини-аппа для площадки %q", venue)
	}
	return s.c.InitData(ctx, app)
}

func (s sessionAdapter) Invalidate(venue string) { s.c.Invalidate(venue) }

var miniApps = map[string]tgsession.MiniApp{
	"MRKT":    tgsession.MRKT,
	"Portals": tgsession.Portals,
}

// startSession brings the Telegram account online when it is configured.
//
// A missing or unusable session is never fatal: the desk still trades on
// Tonnel, and the venues fall back to whatever initData was pasted into the
// environment. It is reported loudly in /status instead, because running
// without it is the configuration that got the account banned.
func (a *App) startSession(ctx context.Context) {
	if a.session == nil {
		return
	}
	if !a.session.LoggedIn() {
		a.notify("🔑 Telegram-сессия не заведена. Один раз выполни <code>floorline login</code> — " +
			"без неё MRKT и Portals читаются как голое API, за что уже прилетал бан.")
		return
	}
	if err := a.session.Start(ctx); err != nil {
		a.notify("🔑 Telegram-сессия не поднялась: " + err.Error())
	}
}

// venuesLine reports which marketplaces the desk can actually trade on.
//
// Being able to read a venue and being able to buy on it are different states,
// and the difference has to be visible here rather than discovered at the
// moment a purchase is supposed to happen.
func (a *App) venuesLine() string {
	if a.venues == nil {
		return "Площадки: только Tonnel"
	}
	ready, pending := a.venues.Buyable(context.Background())
	line := "Покупка доступна: " + strings.Join(ready, ", ")
	if len(ready) == 0 {
		line = "Покупка недоступна нигде"
	}
	if len(pending) > 0 {
		line += " · ждут снятия запроса: " + strings.Join(pending, ", ")
	}
	return line
}

// sessionLine describes the account for /status.
func (a *App) sessionLine() string {
	if a.session == nil {
		return "Telegram-сессия не настроена — MRKT и Portals идут без мини-аппа"
	}
	st := a.session.Status()
	switch {
	case !st.Configured:
		return "Telegram-сессия не настроена (нет TELEGRAM_APP_ID/HASH)"
	case !st.LoggedIn:
		return "Telegram-сессия: нет входа — выполни floorline login"
	case st.Err != "":
		return "Telegram-сессия отвалилась: " + st.Err
	case !st.Connected:
		return "Telegram-сессия: подключаюсь…"
	}
	parts := make([]string, 0, len(st.Ages))
	for venue, age := range st.Ages {
		parts = append(parts, fmt.Sprintf("%s %s назад", venue, dur(age)))
	}
	if len(parts) == 0 {
		return "Telegram-сессия: на связи, мини-аппы ещё не открывались"
	}
	return "Telegram-сессия: на связи · initData " + strings.Join(parts, " · ")
}
