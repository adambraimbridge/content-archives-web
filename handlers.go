package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"

	oktaUtils "github.com/Financial-Times/content-archives-web/utils"
	"github.com/gorilla/mux"
)

const (
	indexTemplate = "templates/index.tmpl.html"
)

var (
	state = "ApplicationState"
	nonce = "NonceNotSet"
)

// Exchange structure
type Exchange struct {
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
	AccessToken      string `json:"access_token,omitempty"`
	TokenType        string `json:"token_type,omitempty"`
	ExpiresIn        int    `json:"expires_in,omitempty"`
	Scope            string `json:"scope,omitempty"`
	IDToken          string `json:"id_token,omitempty"`
	RefreshToken     string `json:"refresh_token,omitempty"`
}

// Handler struct containing handlers configuration
type Handler struct {
	s3Service S3Service
	tmpl      *template.Template
	config    HandlerConfig
}

// HandlerConfig struct containing configuration variable used across the different handler functions,
// mainly used for okta authentication
type HandlerConfig struct {
	oktaClientID     string
	oktaClientSecret string
	oktaScope        string
	issuer           string
	sessionKey       string
	callbackURL      string
}

// NewHandler returns a new Handler instance
func NewHandler(s3Service S3Service, config HandlerConfig) Handler {
	tmpl := template.Must(template.ParseFiles(indexTemplate))
	return Handler{s3Service, tmpl, config}
}

// HomepageHandler serves the homepage content
func (h *Handler) HomepageHandler(w http.ResponseWriter, r *http.Request) {
	zipFiles, err := h.s3Service.RetrieveArchivesFromS3()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Unable to get archives list from S3"))
	}
	h.tmpl.Execute(w, zipFiles)
}

// DownloadHandler starts the download of the specified file in request
func (h *Handler) DownloadHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]
	if name == "" || len(strings.TrimSpace(name)) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("Please specify the name of the file"))
	}

	url, err := h.s3Service.GetDownloadURLForFile(name)
	if err != nil {
		log.Println("Unable to download archive from S3", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Unable to download archive from S3"))
	}

	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

// AuthHandler handler for guarding paths requiting authentication
func (h *Handler) AuthHandler(f http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAuthenticated(r, h.config.sessionKey) {
			log.Println("Is Authenticated\\n")
			idToken, _ := getSessionKey(r, h.config.sessionKey, "id_token")
			accessToken, _ := getSessionKey(r, h.config.sessionKey, "access_token")
			nonce, _ := getSessionKey(r, h.config.sessionKey, "nonce")

			_, err := oktaUtils.VerifyTokens(idToken, accessToken, nonce, h.config.oktaClientID, h.config.issuer)

			if err != nil {
				refreshToken, _ := getSessionKey(r, h.config.sessionKey, "refresh_token")
				e := h.retrieveRefreshToken(refreshToken, r)

				_, err = oktaUtils.VerifyTokens(e.IDToken, e.AccessToken, nonce, h.config.oktaClientID, h.config.issuer)

				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}

				createSession(w, r, h.config.sessionKey, e)
			}

			var handler http.Handler = http.HandlerFunc(f)

			handler.ServeHTTP(w, r)
		} else {
			log.Println("Not Authenticated redirecting to login\\n")
			http.Redirect(w, r, "/login", http.StatusMovedPermanently)
		}
	})
}

func isAuthenticated(r *http.Request, sessionKey string) bool {
	session, err := sessionStore.Get(r, sessionKey)

	if err != nil {
		return false
	}

	if session.Values["id_token"] == nil || session.Values["id_token"] == "" {
		return false
	}

	if session.Values["access_token"] == nil || session.Values["access_token"] == "" {
		return false
	}

	if session.Values["refresh_token"] == nil || session.Values["refresh_token"] == "" {
		return false
	}

	return true
}

func getSessionKey(r *http.Request, sessionKey string, key string) (string, error) {
	session, err := sessionStore.Get(r, sessionKey)

	if err != nil {
		return "", err
	}

	if session.Values[key] == nil {
		return "", nil
	}

	return session.Values[key].(string), nil
}

func createSession(w http.ResponseWriter, r *http.Request, sessionKey string, e Exchange) error {
	session, err := sessionStore.Get(r, sessionKey)

	if err != nil {
		return err
	}

	session.Values["id_token"] = e.IDToken
	session.Values["access_token"] = e.AccessToken
	session.Values["refresh_token"] = e.RefreshToken

	session.Save(r, w)

	return nil
}

// LoginHandler handler initiating the login workflow with Okta
func (h *Handler) LoginHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Creating Login request\\n")
	nonce, _ := oktaUtils.GenerateNonce()
	var redirectPath string

	q := r.URL.Query()
	q.Add("client_id", h.config.oktaClientID)
	q.Add("response_type", "code")
	q.Add("response_mode", "query")
	q.Add("scope", h.config.oktaScope)
	q.Add("redirect_uri", h.config.callbackURL)
	q.Add("state", state)
	q.Add("nonce", nonce)

	redirectPath = h.config.issuer + "/v1/authorize?" + q.Encode()

	session, err := sessionStore.Get(r, h.config.sessionKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	session.Values["nonce"] = nonce
	session.Save(r, w)

	http.Redirect(w, r, redirectPath, http.StatusMovedPermanently)
}

// AuthCodeCallbackHandler is the default callback handler after successful login with Okta
func (h *Handler) AuthCodeCallbackHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Enter AuthCodeCallbackHandler\\n")
	code := r.URL.Query().Get("code")
	// Check the state that was returned in the query string is the same as the above state
	if r.URL.Query().Get("state") != state {
		log.Println("The state was not as expected")
		http.Error(w, "internal error occurred while authenticating with okta", http.StatusInternalServerError)
		return
	}
	// Make sure the code was provided
	if code == "" {
		http.Error(w, "internal error occurred while authenticating with okta", http.StatusInternalServerError)
		log.Println("The code was not returned or is not accessible")
		return
	}

	e := h.retrieveAuthToken(code, r)
	nonce, err := getSessionKey(r, h.config.sessionKey, "nonce")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	_, err = oktaUtils.VerifyTokens(e.IDToken, e.AccessToken, nonce, h.config.oktaClientID, h.config.issuer)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	createSession(w, r, h.config.sessionKey, e)

	http.Redirect(w, r, "/", http.StatusMovedPermanently)
}

func (h *Handler) retrieveAuthToken(code string, r *http.Request) Exchange {
	log.Println("Retrieve AuthToken\\n")
	q := r.URL.Query()
	q.Add("grant_type", "authorization_code")
	q.Add("code", code)

	return h.retrieveToken(q)
}

func (h *Handler) retrieveRefreshToken(refresToken string, r *http.Request) Exchange {
	log.Println("Retrieve RefreshToken\\n")
	q := r.URL.Query()
	q.Add("grant_type", "refresh_token")
	q.Add("refresh_token", refresToken)

	return h.retrieveToken(q)
}

// private function for login
func (h *Handler) retrieveToken(q url.Values) Exchange {
	authHeader := base64.StdEncoding.EncodeToString(
		[]byte(h.config.oktaClientID + ":" + h.config.oktaClientSecret))

	q.Add("redirect_uri", h.config.callbackURL)

	url := h.config.issuer + "/v1/token?" + q.Encode()

	req, _ := http.NewRequest("POST", url, bytes.NewReader([]byte("")))
	header := req.Header
	header.Add("Authorization", "Basic "+authHeader)
	header.Add("Accept", "application/json")
	header.Add("Content-Type", "application/x-www-form-urlencoded")
	header.Add("Connection", "close")
	header.Add("Content-Length", "0")

	client := &http.Client{}
	resp, _ := client.Do(req)
	body, _ := ioutil.ReadAll(resp.Body)
	defer resp.Body.Close()
	var exchange Exchange
	json.Unmarshal(body, &exchange)

	return exchange
}
