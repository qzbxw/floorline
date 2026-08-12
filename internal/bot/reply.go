package bot

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
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
	// Preview lets Telegram unfurl the first link in the text. It is off by
	// default because a preview under a data view is clutter, and on for signal
	// cards because there the preview *is* the picture of the gift.
	Preview bool
}

// WithPreview enables the link preview for this reply.
func (r Reply) WithPreview() Reply { r.Preview = true; return r }

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

// NFTPreviewURL is the gift's canonical Telegram collectible link, which the
// client unfurls into a picture of the actual item with its model, backdrop and
// symbol.
//
// This is the only way to see the thing before buying it. The Tonnel deep link
// cannot do it — a mini-app URL has nothing to unfurl — so a card built around
// it asked the desk to judge a Black Mamba from a number, on a market where
// half of what people pay for is how the gift looks.
//
// The slug is the collection name with everything but letters and digits
// removed: "Vice Cream" #49968 becomes ViceCream-49968. Tonnel's API carries no
// slug field, so this is derived rather than read. A collection whose real slug
// differs simply produces a link that does not unfurl, which costs a picture
// and nothing else.
func NFTPreviewURL(collection string, giftNum int64) string {
	if giftNum <= 0 {
		return ""
	}
	var slug strings.Builder
	for _, r := range collection {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			slug.WriteRune(r)
		}
	}
	if slug.Len() == 0 {
		return ""
	}
	return "https://t.me/nft/" + slug.String() + "-" + strconv.FormatInt(giftNum, 10)
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
