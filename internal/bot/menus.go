package bot

// The keyboards.
//
// Two rules shape all of them.
//
// First, width. Telegram lays inline buttons out in rows, and a keyboard of
// one-button rows becomes a column the operator has to scroll past to reach the
// message above it — which on a phone is the message they are trying to read.
// Three short buttons in a row occupy the same height as one long one, so
// labels are kept to roughly ten characters and rows are filled.
//
// Second, depth. Every tap that only leads to more taps is a tap that did
// nothing. The submenus that existed to hold two entries are gone; what is left
// either shows data or switches something.
const (
	cbMenu   = "m_menu"   // home dashboard
	cbMarket = "m_market" // market tools
	cbAlerts = "m_alerts" // alerts and watchlist
	cbMore   = "m_more"   // limits, session, status, help
	cbAuto   = "m_auto"   // auto-buy panel (built by the backend)
	cbHelp   = "m_help"   // command reference
	cbPick   = "m_pick"   // collection picker for a drill-down view
	cbHowTo  = "m_howto"  // the few screens that explain a typed command
)

// nav is the trailing row every data view gets: one step back and one step
// home. It is a single row rather than two so a list of positions does not end
// in more navigation than content.
func nav(r Reply, target, data, label string) Reply {
	if r.Empty() {
		return r
	}
	if target == "" || target == cbMenu {
		return r.WithRow(Callback("🏠 Домой", cbMenu, ""))
	}
	return r.WithRow(Callback(label, target, data), Callback("🏠 Домой", cbMenu, ""))
}

// back is nav for targets that carry no payload.
func back(r Reply, target, label string) Reply { return nav(r, target, "", label) }

// backWith is nav for targets that need one, such as the collection picker.
func backWith(r Reply, target, data, label string) Reply { return nav(r, target, data, label) }

// HomeRows exposes the dashboard grid to the backend, which owns the text above
// it. The keyboard stays in this package so every screen's navigation is
// declared in one place.
func HomeRows() [][]Button { return homeKeyboard() }

// homeKeyboard is the dashboard's grid.
//
// The order is by how often each is used, not by category: a session, a scan
// and the open positions are the working day; limits and help are not. The text
// above it is built from live state, because a home screen that says the same
// thing every time is a splash screen.
func homeKeyboard() [][]Button {
	return [][]Button{
		{Callback("⚔️ Сессия", cbRefresh, "trade"), Callback("🔭 Скан", cbRefresh, "scan"), Callback("📍 Лоты", cbRefresh, "pos")},
		{Callback("⚡️ Автобай", cbAuto, ""), Callback("💰 PnL", cbRefresh, "pnl"), Callback("📊 Рынок", cbMarket, "")},
		{Callback("⏰ Алерты", cbAlerts, ""), Callback("⚙️ Ещё", cbMore, ""), Callback("📚 Помощь", cbHelp, "")},
	}
}

// marketMenu is the four things that need a collection or a gift id chosen
// before they can show anything.
func marketMenu() Reply {
	return Reply{
		Text: "📊 <b>Рынок</b>",
		Rows: [][]Button{
			{Callback("📈 Флор", cbPick, "floor"), Callback("📖 Стакан", cbPick, "book"), Callback("🕒 Сделки", cbPick, "hist")},
			{Callback("💹 GRAM", cbRefresh, "gram"), Callback("🔭 Скан", cbRefresh, "scan"), Callback("💰 Оценка", cbMarket, "val")},
			{Callback("🏠 Домой", cbMenu, "")},
		},
	}
}

// alertsMenu is the watchlist and the mutes.
func alertsMenu() Reply {
	return Reply{
		Text: "⏰ <b>Алерты</b>\n\nПодписки на модели и тишина по коллекциям.",
		Rows: [][]Button{
			{Callback("👁 Подписки", cbRefresh, "watchlist"), Callback("📌 Добавить", cbHowTo, "watch")},
			{Callback("🔕 Заглушить", cbHowTo, "mute"), Callback("🏠 Домой", cbMenu, "")},
		},
	}
}

// moreMenu holds what is touched rarely: money limits, the session credential,
// process health and the command reference.
func moreMenu() Reply {
	return Reply{
		Text: "⚙️ <b>Ещё</b>",
		Rows: [][]Button{
			{Callback("💰 Лимиты", cbRefresh, "limits"), Callback("🔄 Статус", cbRefresh, "status"), Callback("💵 Баланс", cbRefresh, "balance")},
			{Callback("📊 Обзор", cbRefresh, "portfolio"), Callback("🔐 Сессия", cbHowTo, "auth"), Callback("📚 Помощь", cbHelp, "")},
			{Callback("🏠 Домой", cbMenu, "")},
		},
	}
}

// howTo covers the handful of actions that genuinely need something typed: a
// collection name with a slash in it, a gift id, a credential. Each screen is
// the shortest thing that gets the operator to a working command.
func howTo(action string) Reply {
	var text, backTo string
	switch action {
	case "watch":
		backTo = cbAlerts
		text = "📌 <b>Подписаться</b>\n\n" +
			"<code>/watch Plush Pepe / Pink Diamond</code>\n" +
			"<code>/watch Plush Pepe / Pink Diamond 50</code> — только дешевле 50\n\n" +
			"<i>Слеш обязателен: и коллекция, и модель бывают из двух слов.</i>"
	case "mute":
		backTo = cbAlerts
		text = "🔕 <b>Заглушить</b>\n\n" +
			"<code>/mute Plush Pepe</code> — на час\n" +
			"<code>/mute Plush Pepe 4</code> — на четыре\n" +
			"<code>/mute Plush Pepe / Pink Diamond 4</code> — одну модель\n\n" +
			"Вернуть звук: <code>/unmute Plush Pepe</code>"
	case "val":
		backTo = cbMarket
		text = "💰 <b>Оценка лота</b>\n\n" +
			"Проще всего — <b>Share</b> на гифте в мини-аппе Tonnel и кинуть ссылку сюда. " +
			"Просто сообщением, подпись не мешает.\n\n" +
			"Или руками: <code>/val 10368454</code>."
	case "auth":
		backTo = cbMore
		text = "🔐 <b>Сессия Tonnel</b>\n\n" +
			"Открой мини-апп с DevTools и в консоли выполни:\n" +
			"<code>copy(Telegram.WebApp.initData)</code>\n\n" +
			"Потом пришли сюда <code>/auth &lt;строка&gt;</code>.\n\n" +
			"<i>Сообщение удалю сразу — в истории чата оно не останется.</i>"
	default:
		return moreMenu()
	}
	return Reply{Text: text, Rows: [][]Button{
		{Callback("🔙 Назад", backTo, ""), Callback("🏠 Домой", cbMenu, "")},
	}}
}

// helpMenuReply is the command reference.
//
// It is grouped by what the operator is trying to do rather than by which
// package implements it, and every line is command-first so the eye can scan
// the left edge instead of reading prose.
func helpMenuReply() Reply {
	return Reply{
		Text: "📚 <b>Команды</b>\n\n" +

			"<b>Торговать</b>\n" +
			"<code>/trade</code> · сессия по ликвидным парам\n" +
			"<code>/trade off</code> · выйти из сессии\n" +
			"<code>/scan [коллекция] [3-5] [сколько]</code> · скан рынка: диапазон цен и лимит\n" +
			"<code>/val ID</code> · оценка лота — или кинь ссылку из Tonnel\n\n" +

			"<b>Смотреть рынок</b>\n" +
			"<code>/floor Коллекция</code> · модели и полы\n" +
			"<code>/book Коллекция / Модель</code> · лесенка асков\n" +
			"<code>/hist Коллекция / Модель</code> · реальные сделки\n" +
			"<code>/gram</code> · курс GRAM и отставание флоров\n\n" +

			"<b>Свои лоты</b>\n" +
			"<code>/pos</code> · позиции с переоценкой\n" +
			"<code>/portfolio</code> · держать / переставить / сокращать\n" +
			"<code>/pnl</code> · прибыль в GRAM и в долларах\n" +
			"<code>/relist ID</code> · переставить по текущему стакану\n" +
			"<code>/advice ID</code> · как выходить из одной позиции\n" +
			"<code>/history ID</code> · вся жизнь позиции\n" +
			"<code>/exit ID цена</code> · ручной выход\n" +
			"<code>/cost ID цена</code> · задать себестоимость\n" +
			"<code>/balance</code>\n\n" +

			"<b>Автобай</b>\n" +
			"<code>/autobuy</code> · что включено и что мешает купить\n" +
			"<code>/arm</code> · <code>/disarm</code> · покупка\n" +
			"<code>/resell on|off</code> · продавать ли самому\n" +
			"<code>/limits</code> · <code>/limits set max_ticket 50</code>\n\n" +

			"<b>Алерты</b>\n" +
			"<code>/watch Коллекция / Модель [цена]</code>\n" +
			"<code>/unwatch</code> · <code>/watchlist</code>\n" +
			"<code>/mute Коллекция [часы]</code> · <code>/unmute</code>\n\n" +

			"<b>Служебное</b>\n" +
			"<code>/status</code> · поллеры, свежесть данных, прогрев\n" +
			"<code>/auth строка</code> · заменить протухшую сессию\n\n" +

			"<i>Коллекция и модель разделяются слешем — обе бывают из двух слов.</i>",
		Rows: [][]Button{
			{Callback("🏠 Домой", cbMenu, "")},
		},
	}
}
