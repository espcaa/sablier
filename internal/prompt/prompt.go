package prompt

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func Normalize(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

// unike hash per prompt
func Key(salt []byte, session string, at time.Time) []byte {
	m := hmac.New(sha256.New, salt)
	m.Write([]byte(session + "\x00" + strconv.FormatInt(at.UnixMilli(), 10)))
	return m.Sum(nil)
}

func Hash(key []byte, text string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(Normalize(text)))
	return hex.EncodeToString(m.Sum(nil))
}

// whole prompt matches only
var approvals = map[string]bool{}

func init() {
	for _, s := range []string{
		"y", "yes", "yep", "yeah", "yup", "sure", "ok", "okay", "k", "kk",
		"go", "go ahead", "go for it", "do it", "continue", "proceed", "keep going",
		"yes please", "please continue", "lgtm", "looks good", "sounds good", "ship it",
	} {
		approvals[stripPunct(s)] = true
	}
}

func ApprovalLike(text string) bool {
	return approvals[stripPunct(Normalize(text))]
}

func stripPunct(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsSpace(r) {
			return r
		}
		if r == '-' || r == '\'' {
			return ' '
		}
		return -1
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
