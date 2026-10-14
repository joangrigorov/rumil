package ghapp

import "strings"

// NormalizeLogin turns a configured handle or a login GitHub reported into the
// account name they share: "@rumil", "rumil" and "rumil[bot]" all become
// "rumil". One definition of the comparison keeps the webhook's review-request
// check and the comment lookups from drifting apart.
func NormalizeLogin(handle string) string {
	login := strings.ToLower(strings.TrimSpace(handle))
	login = strings.TrimPrefix(login, "@")
	login = strings.TrimSuffix(login, "[bot]")
	return login
}

// sameLogin reports whether two logins name the same account.
func sameLogin(a, b string) bool {
	return NormalizeLogin(a) != "" && NormalizeLogin(a) == NormalizeLogin(b)
}
