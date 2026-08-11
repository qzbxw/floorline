package bot

import (
	"regexp"
	"strconv"
	"strings"
)

// giftLink matches the deep link Tonnel's mini app puts on the clipboard when
// a gift is shared. The mini app never shows the numeric id anywhere, so the
// share link is the only way to name a lot from a phone — and it arrives with
// a "Check out this gift!" caption glued to it.
//
// Both the /gift?startapp=<id> form and the plain startapp=<id> query are
// accepted, with an optional prefix on the payload (startapp=gift_123), because
// deep-link payloads have changed shape before.
var giftLink = regexp.MustCompile(`(?i)(?:startapp=|tonnel[^\s]*/gift/)(?:[a-z_-]*?)(\d{4,})`)

// bareID matches a message that is nothing but a gift id.
var bareID = regexp.MustCompile(`^\d{4,}$`)

// ParseGiftRef pulls a Tonnel gift id out of whatever the operator pasted: a
// share link, a link with its caption, or the bare number. The second return
// reports whether anything usable was found.
func ParseGiftRef(text string) (int64, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, false
	}
	if bareID.MatchString(text) {
		id, err := strconv.ParseInt(text, 10, 64)
		return id, err == nil
	}
	if m := giftLink.FindStringSubmatch(text); m != nil {
		id, err := strconv.ParseInt(m[1], 10, 64)
		return id, err == nil && id > 0
	}
	return 0, false
}
