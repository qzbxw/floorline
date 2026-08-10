package bot

import (
	"fmt"
	"strconv"
)

// Button is one inline keyboard button. A button either routes a callback
// (Unique + Data) or opens a link (URL).
type Button struct {
	Label  string
	Unique string
	Data   string
	URL    string
}

// Reply is what a Backend hands back: message text plus an optional keyboard.
//
// Returning the keyboard alongside the text is what lets list views carry their
// own actions — a position can offer a Relist button instead of printing a
// command for the operator to retype on a phone.
type Reply struct {
	Text string
	Rows [][]Button
}

// Text builds a plain reply. It takes the finished string rather than a format
// so that vet does not mistake every caller for a printf wrapper.
func Text(text string) Reply { return Reply{Text: text} }

// Textf builds a formatted reply.
func Textf(format string, args ...any) Reply { return Reply{Text: fmt.Sprintf(format, args...)} }

// Empty reports whether there is nothing to send.
func (r Reply) Empty() bool { return r.Text == "" && len(r.Rows) == 0 }

// WithRow appends a keyboard row, ignoring empty ones.
func (r Reply) WithRow(buttons ...Button) Reply {
	if len(buttons) > 0 {
		r.Rows = append(r.Rows, buttons)
	}
	return r
}

// Callback builds a button that routes back into the bot.
func Callback(label, unique string, data ...any) Button {
	return Button{Label: label, Unique: unique, Data: joinData(data...)}
}

// Link builds a button that opens a URL.
func Link(label, url string) Button {
	return Button{Label: label, URL: url}
}

// TonnelGiftURL deep-links straight to a listing inside the Tonnel mini app.
// A manual trader wants to see the actual lot, not just our summary of it.
func TonnelGiftURL(giftID int64) string {
	return "https://t.me/tonnel_network_bot/gift?startapp=" + strconv.FormatInt(giftID, 10)
}

func joinData(parts ...any) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "|"
		}
		switch v := p.(type) {
		case string:
			out += v
		case int64:
			out += strconv.FormatInt(v, 10)
		case int:
			out += strconv.Itoa(v)
		default:
			out += fmt.Sprint(v)
		}
	}
	return out
}
