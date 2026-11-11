package authelia

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// UsersDBFile is the users database's file name inside the claim. Authelia
// opens exactly this path (configuration.yml's authentication_backend.file.path)
// and refuses to start when it is missing, so box writes it before the
// Deployment exists.
const UsersDBFile = "users_database.yml"

// User is one row of the users database: the subset of Authelia's row schema
// box writes and reads back.
type User struct {
	Login       string
	DisplayName string
	// Password is the PHC digest, never a password.
	Password string
	// Groups is preserved when a row has them; box writes none of its own.
	// They matter only to access_control rules that name a group, and box's
	// rules name a user.
	Groups []string
}

// Users is a users database: rows keyed by login, and the order they were read
// in, so a file box did not change is written back byte for byte.
type Users struct {
	order []string
	rows  map[string]User
}

// NewUsers is an empty database.
func NewUsers() *Users { return &Users{rows: map[string]User{}} }

// Len is how many rows there are.
func (u *Users) Len() int { return len(u.order) }

// Get returns one row.
func (u *Users) Get(login string) (User, bool) {
	row, ok := u.rows[login]
	return row, ok
}

// Put adds or replaces a row, keeping the order a re-read file had and
// appending a new login at the end.
func (u *Users) Put(row User) {
	if _, ok := u.rows[row.Login]; !ok {
		u.order = append(u.order, row.Login)
	}
	u.rows[row.Login] = row
}

// Logins is the rows' logins in file order.
func (u *Users) Logins() []string {
	out := make([]string, len(u.order))
	copy(out, u.order)
	return out
}

// ErrUsersFormat says the users database is not the shape Authelia accepts: a
// single `users:` mapping whose every row has displayname and password.
var ErrUsersFormat = errors.New("users database")

// ParseUsers reads the file box writes.
//
// It is deliberately a small reader for one shape rather than a YAML
// dependency: the file is box's, the two fields that matter are a scalar map,
// and a row box cannot read is a row Authelia would refuse to load. A file
// written in flow style or with anchors is refused by name instead of being
// half-understood and rewritten.
func ParseUsers(data []byte) (*Users, error) {
	out := NewUsers()
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var (
		sawUsers bool
		current  *User
	)
	flush := func() error {
		if current == nil {
			return nil
		}
		if strings.TrimSpace(current.DisplayName) == "" {
			return fmt.Errorf("%w: user %q has no displayname: Authelia refuses a row without one, and one bad row fails the whole file", ErrUsersFormat, current.Login)
		}
		if !DigestPattern.MatchString(current.Password) {
			return fmt.Errorf("%w: user %q has no argon2 digest: Authelia refuses a row without one, and one bad row fails the whole file", ErrUsersFormat, current.Login)
		}
		out.Put(*current)
		current = nil
		return nil
	}
	for i, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		body := strings.TrimSpace(line)
		switch {
		case indent == 0:
			key, rest, ok := strings.Cut(body, ":")
			if !ok || key != "users" || strings.TrimSpace(strings.TrimPrefix(rest, " ")) != "" {
				return nil, fmt.Errorf("%w: line %d: the file is one `users:` mapping, one row per login", ErrUsersFormat, i+1)
			}
			sawUsers = true
		case indent == 2:
			if err := flush(); err != nil {
				return nil, err
			}
			login, rest, ok := strings.Cut(body, ":")
			login = strings.TrimSpace(login)
			if !ok || login == "" || strings.TrimSpace(rest) != "" {
				return nil, fmt.Errorf("%w: line %d: %q is not a login", ErrUsersFormat, i+1, body)
			}
			current = &User{Login: unquote(login)}
		case indent == 4 && current != nil:
			key, value, ok := strings.Cut(body, ":")
			if !ok {
				return nil, fmt.Errorf("%w: line %d: %q is not a field", ErrUsersFormat, i+1, body)
			}
			key, value = strings.TrimSpace(key), unquote(strings.TrimSpace(value))
			switch key {
			case "displayname":
				current.DisplayName = value
			case "password":
				current.Password = value
			case "groups":
				current.Groups = flowList(value)
			case "email", "disabled":
				// Accepted and dropped: box writes neither, and Authelia does
				// not require them for a password login. Dropping one is why
				// this parser is only for a file box wrote.
			default:
				return nil, fmt.Errorf("%w: line %d: field %q is not one box writes", ErrUsersFormat, i+1, key)
			}
		default:
			return nil, fmt.Errorf("%w: line %d: unexpected indentation", ErrUsersFormat, i+1)
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if !sawUsers {
		return nil, fmt.Errorf("%w: no `users:` key", ErrUsersFormat)
	}
	return out, nil
}

// Render writes the database back.
//
// A login's row is its display name and its digest, in that order, as
// single-quoted scalars where a YAML reader would otherwise have to guess
// (a digest starts with `$`, which is not special but reads badly bare).
// Sorting makes a file box rewrote byte-identical across runs.
func (u *Users) Render() []byte {
	logins := u.Logins()
	sort.Strings(logins)
	var b strings.Builder
	b.WriteString("## box: root's login for this seedbox. One row per login; the digest is argon2id.\n")
	b.WriteString("## Written by `box init`; Authelia watches this file. Mode 0600, owned by the\n")
	b.WriteString("## uid the login runs as.\n")
	b.WriteString("users:\n")
	for _, login := range logins {
		row := u.rows[login]
		fmt.Fprintf(&b, "  %s:\n", yamlKey(login))
		fmt.Fprintf(&b, "    displayname: %s\n", yamlScalar(row.DisplayName))
		fmt.Fprintf(&b, "    password: %s\n", yamlScalar(row.Password))
		if len(row.Groups) > 0 {
			fmt.Fprintf(&b, "    groups: [%s]\n", strings.Join(row.Groups, ", "))
		}
	}
	return []byte(b.String())
}

// yamlKey quotes a login only when it has to be: Authelia's usernames are
// plain in practice, and quoting every one makes the file harder to read.
func yamlKey(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if r != '-' && r != '_' && r != '.' && r != '@' &&
			(r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return yamlScalar(s)
		}
	}
	return s
}

// yamlScalar single-quotes a value: a digest holds `$` and `,` and a display
// name may hold anything, and single quotes are the one YAML form that needs
// no escaping except the quote itself.
func yamlScalar(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `\"`, `"`)
	}
	return s
}

// flowList reads Authelia's own `groups: [a, b]` spelling, which is the only
// list form a box-written file has.
func flowList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(unquote(strings.TrimSpace(part))); p != "" {
			out = append(out, p)
		}
	}
	return out
}
