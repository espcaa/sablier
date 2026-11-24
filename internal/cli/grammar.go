package cli

func Noun(n int64, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
