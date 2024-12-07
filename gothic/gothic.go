/*
Package gothic wraps common behaviour when using Goth. This makes it quick, and easy, to get up
and running with Goth. Of course, if you want complete control over how things flow, in regard
to the authentication process, feel free and use Goth directly.

See https://github.com/markbates/goth/blob/master/examples/main.go to see this in action.
*/
package gothic

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/andreimerlescu/goth"
	"github.com/go-chi/chi/v5"
	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
)

type contextKey string

var (
	keySet       = false
	SessionName  = "_gothic_session"
	defaultStore sessions.Store
	Store        sessions.Store

	ErrSessionNotFound    = errors.New("could not find a matching session for this request")
	ErrProviderRequired   = errors.New("you must select a provider")
	ErrStateTokenMismatch = errors.New("state token mismatch")
	ErrContextTimeout     = errors.New("operation timed out")
	ErrContextCanceled    = errors.New("operation was canceled")

	timeoutKey     contextKey = "timeout"
	providerKey    contextKey = "provider"
	defaultTimeout            = 30 * time.Second
)

// ProviderParamKey can be used as a key in context when passing in a provider
const ProviderParamKey int = iota

func init() {
	if len(os.Getenv("SESSION_SECRET")) > 0 {
		err := UseCookies([]byte(os.Getenv("SESSION_SECRET")), &sessions.Options{HttpOnly: true})
		if err != nil {
			keySet = true
		}
	}
}

// UseCookies assigns the sessions.Store to sessions.NewCookieStore using your provided key.
// You supply a pointer to the session.Options into gothic.
func UseCookies(key []byte, opts *sessions.Options) error {
	cookieStore := sessions.NewCookieStore(key)
	cookieStore.Options = opts
	Store = cookieStore
	defaultStore = Store
	keySet = true
	return nil
}

// UseFilesystem assigns the sessions.Store to sessions.NewFilesystemStore using your path and
// provided key. You supply a pointer to your sessions.Options into gothic.
func UseFilesystem(path string, key []byte, opts *sessions.Options) error {
	fsStore := sessions.NewFilesystemStore(path, key)
	fsStore.Options = opts
	Store = fsStore
	defaultStore = Store
	keySet = true
	return nil
}

/*
BeginAuthHandler is a convenience handler for starting the authentication process.
It expects to be able to get the name of the provider from the query parameters
as either "provider" or ":provider".

BeginAuthHandler will redirect the user to the appropriate authentication end-point
for the requested provider.

See https://github.com/markbates/goth/blob/master/examples/main.go to see this in action.
*/
func BeginAuthHandler(res http.ResponseWriter, req *http.Request) {
	authURL, err := GetAuthURL(res, req)
	if err != nil {
		res.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintln(res, err)
		return
	}

	http.Redirect(res, req, authURL, http.StatusTemporaryRedirect)
}

// SetState sets the state string associated with the given request.
// If no state string is associated with the request, one will be generated.
// This state is sent to the provider and can be retrieved during the
// callback.
var SetState = func(req *http.Request) string {
	state := req.URL.Query().Get("state")
	if len(state) > 0 {
		return state
	}

	// If a state query param is not passed in, generate a random
	// base64-encoded nonce so that the state on the auth URL
	// is unguessable, preventing CSRF attacks, as described in
	//
	// https://auth0.com/docs/protocols/oauth2/oauth-state#keep-reading
	nonceBytes := make([]byte, 64)
	_, err := io.ReadFull(rand.Reader, nonceBytes)
	if err != nil {
		panic("gothic: source of randomness unavailable: " + err.Error())
	}
	return base64.URLEncoding.EncodeToString(nonceBytes)
}

// GetState gets the state returned by the provider during the callback.
// This is used to prevent CSRF attacks, see
// http://tools.ietf.org/html/rfc6749#section-10.12
var GetState = func(req *http.Request) string {
	params := req.URL.Query()
	if params.Encode() == "" && req.Method == http.MethodPost {
		return req.FormValue("state")
	}
	return params.Get("state")
}

/*
GetAuthURL starts the authentication process with the requested provided.
It will return a URL that should be used to send users to.

It expects to be able to get the name of the provider from the query parameters
as either "provider" or ":provider".

I would recommend using the BeginAuthHandler instead of doing all of these steps
yourself, but that's entirely up to you.
*/
func GetAuthURL(res http.ResponseWriter, req *http.Request) (string, error) {
	if !keySet && defaultStore == Store {
		fmt.Println("goth/gothic: no SESSION_SECRET environment variable is set. The default cookie store is not available and any calls will fail. Ignore this warning if you are using a different store.")
	}

	providerName, err := GetProviderName(req)
	if err != nil {
		return "", err
	}

	provider, err := goth.GetProvider(providerName)
	if err != nil {
		return "", err
	}
	sess, err := provider.BeginAuth(SetState(req))
	if err != nil {
		return "", err
	}

	authURL, err := sess.GetAuthURL()
	if err != nil {
		return "", err
	}

	err = StoreInSession(providerName, sess.Marshal(), req, res)

	if err != nil {
		return "", err
	}

	return authURL, err
}

/*
CompleteUserAuth does what it says on the tin. It completes the authentication
process and fetches all the basic information about the user from the provider.

It expects to be able to get the name of the provider from the query parameters
as either "provider" or ":provider".

See https://github.com/markbates/goth/blob/master/examples/main.go to see this in action.
*/
var CompleteUserAuth = func(res http.ResponseWriter, req *http.Request) (goth.User, error) {
	if !keySet && defaultStore == Store {
		fmt.Println("goth/gothic: no SESSION_SECRET environment variable is set. The default cookie store is not available and any calls will fail. Ignore this warning if you are using a different store.")
	}

	providerName, err := GetProviderName(req)
	if err != nil {
		return goth.User{}, err
	}

	provider, err := goth.GetProvider(providerName)
	if err != nil {
		return goth.User{}, err
	}

	value, err := GetFromSession(providerName, req)
	if err != nil {
		return goth.User{}, err
	}
	defer Logout(res, req)
	sess, err := provider.UnmarshalSession(value)
	if err != nil {
		return goth.User{}, err
	}

	err = validateState(req, sess)
	if err != nil {
		return goth.User{}, err
	}

	user, err := provider.FetchUser(sess)
	if err == nil {
		// user can be found with existing session data
		return user, err
	}

	params := req.URL.Query()
	if params.Encode() == "" && req.Method == "POST" {
		err := req.ParseForm()
		if err != nil {
			return goth.User{}, err
		}
		params = req.Form
	}

	// get new token and retry fetch
	_, err = sess.Authorize(provider, params)
	if err != nil {
		return goth.User{}, err
	}

	err = StoreInSession(providerName, sess.Marshal(), req, res)

	if err != nil {
		return goth.User{}, err
	}

	gu, err := provider.FetchUser(sess)
	return gu, err
}

// validateState ensures that the state token param from the original
// AuthURL matches the one included in the current (callback) request.
func validateState(req *http.Request, sess goth.Session) error {
	rawAuthURL, err := sess.GetAuthURL()
	if err != nil {
		return err
	}

	authURL, err := url.Parse(rawAuthURL)
	if err != nil {
		return err
	}

	reqState := GetState(req)

	originalState := authURL.Query().Get("state")
	if originalState != "" && (originalState != reqState) {
		return errors.New("state token mismatch")
	}
	return nil
}

// Logout invalidates a user session.
func Logout(res http.ResponseWriter, req *http.Request) error {
	session, err := Store.Get(req, SessionName)
	if err != nil {
		return err
	}
	session.Options.MaxAge = -1
	session.Values = make(map[interface{}]interface{})
	err = session.Save(req, res)
	if err != nil {
		return errors.New("Could not delete user session ")
	}
	return nil
}

// GetProviderName is a function used to get the name of a provider
// for a given request. By default, this provider is fetched from
// the URL query string. If you provide it in a different way,
// assign your own function to this variable that returns the provider
// name for your request.
var GetProviderName = getProviderName

func getProviderName(req *http.Request) (string, error) {

	// try to get it from the url param "provider"
	if p := req.URL.Query().Get("provider"); p != "" {
		return p, nil
	}

	// try to get it from the url param ":provider"
	if p := req.URL.Query().Get(":provider"); p != "" {
		return p, nil
	}

	// try to get it from the context's value of "provider" key
	if p, ok := mux.Vars(req)["provider"]; ok {
		return p, nil
	}

	//  try to get it from the go-context's value of "provider" key
	if p, ok := req.Context().Value("provider").(string); ok {
		return p, nil
	}

	// try to get it from the url param "provider", when req is routed through 'chi'
	if p := chi.URLParam(req, "provider"); p != "" {
		return p, nil
	}

	// try to get it from the go-context's value of providerContextKey key
	if p, ok := req.Context().Value(ProviderParamKey).(string); ok {
		return p, nil
	}

	// As a fallback, loop over the used providers, if we already have a valid session for any provider (ie. user has already begun authentication with a provider), then return that provider name
	providers := goth.GetProviders()
	session, _ := Store.Get(req, SessionName)
	for _, provider := range providers {
		p := provider.Name()
		if session.Values == nil {
			session.Values = make(map[interface{}]interface{})
		}
		value := session.Values[p]
		if _, ok := value.(string); ok {
			return p, nil
		}
	}

	// if not found then return an empty string with the corresponding error
	return "", errors.New("you must select a provider")
}

// GetContextWithProvider returns a new request context containing the provider
func GetContextWithProvider(req *http.Request, provider string) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ProviderParamKey, provider))
}

// StoreInSession stores a specified key/value pair in the session.
func StoreInSession(key string, value string, req *http.Request, res http.ResponseWriter) error {
	session, _ := Store.New(req, SessionName)
	if session.Values == nil {
		session.Values = make(map[interface{}]interface{})
	}

	if err := updateSessionValue(session, key, value); err != nil {
		return err
	}

	return session.Save(req, res)
}

// GetFromSession retrieves a previously-stored value from the session.
// If no value has previously been stored at the specified key, it will return an error.
func GetFromSession(key string, req *http.Request) (string, error) {
	session, _ := Store.Get(req, SessionName)
	value, err := getSessionValue(session, key)
	if err != nil {
		return "", errors.New("could not find a matching session for this request")
	}

	return value, nil
}

func getSessionValue(session *sessions.Session, key string) (string, error) {
	value := session.Values[key]
	if value == nil {
		return "", fmt.Errorf("no session value found for key %s", key)
	}

	rdata := strings.NewReader(value.(string))
	r, err := gzip.NewReader(rdata)
	if err != nil {
		return "", fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func(r *gzip.Reader) {
		err := r.Close()
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "getSessionValue threw gzip.Reader .Close() err: %v", err)
		}
	}(r)

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return "", fmt.Errorf("failed to read gzipped data: %w", err)
	}

	return buf.String(), nil
}

func updateSessionValue(session *sessions.Session, key, value string) error {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := gz.Write([]byte(value)); err != nil {
		return fmt.Errorf("failed to write gzipped data: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("failed to close gzip writer: %w", err)
	}
	if session.Values == nil {
		session.Values = make(map[interface{}]interface{})
	}
	session.Values[key] = b.String()
	return nil
}
