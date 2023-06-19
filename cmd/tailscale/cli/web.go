// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cgi"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/peterbourgon/ff/v3/ffcli"
	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/util/cmpx"
	"tailscale.com/util/groupmember"
	"tailscale.com/version/distro"
)

//go:embed web.html
var webHTML string

//go:embed web.css
var webCSS string

//go:embed auth-redirect.html
var authenticationRedirectHTML string

var tmpl *template.Template

func init() {
	tmpl = template.Must(template.New("web.html").Parse(webHTML))
	template.Must(tmpl.New("web.css").Parse(webCSS))
}

type tmplData struct {
	Profile           tailcfg.UserProfile
	SynologyUser      string
	Status            string
	DeviceName        string
	IP                string
	AdvertiseExitNode bool
	AdvertiseRoutes   string
	LicensesURL       string
	TUNMode           bool
	IsSynology        bool
	DSMVersion        int // 6 or 7, if IsSynology=true
	IsUnraid          bool
	UnraidToken       string
	IPNVersion        string
}

type postedData struct {
	ServerCode        string
	AdvertiseRoutes   string
	AdvertiseExitNode bool
	Reauthenticate    bool
	ForceLogout       bool
}

var webCmd = &ffcli.Command{
	Name:       "web",
	ShortUsage: "web [flags]",
	ShortHelp:  "Run a web server for controlling Mirage",

	LongHelp: strings.TrimSpace(`
"mirage web" runs a webserver for controlling the Mirage daemon.

It's primarily intended for use on Synology, QNAP, and other
NAS devices where a web interface is the natural place to control
Mirage, as opposed to a CLI or a native app.
`),

	FlagSet: (func() *flag.FlagSet {
		webf := newFlagSet("web")
		webf.StringVar(&webArgs.listen, "listen", "localhost:8088", "listen address; use port 0 for automatic")
		webf.BoolVar(&webArgs.cgi, "cgi", false, "run as CGI script")
		return webf
	})(),
	Exec: runWeb,
}

var webArgs struct {
	listen string
	cgi    bool
}

func tlsConfigFromEnvironment() *tls.Config {
	crt := os.Getenv("TLS_CRT_PEM")
	key := os.Getenv("TLS_KEY_PEM")
	if crt == "" || key == "" {
		return nil
	}

	// We support passing in the complete certificate and key from environment
	// variables because pfSense stores its cert+key in the PHP config. We populate
	// TLS_CRT_PEM and TLS_KEY_PEM from PHP code before starting tailscale web.
	// These are the PEM-encoded Certificate and Private Key.

	cert, err := tls.X509KeyPair([]byte(crt), []byte(key))
	if err != nil {
		log.Printf("tlsConfigFromEnvironment: %v", err)

		// Fallback to unencrypted HTTP.
		return nil
	}

	return &tls.Config{Certificates: []tls.Certificate{cert}}
}

func runWeb(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("too many non-flag arguments: %q", args)
	}

	if webArgs.cgi {
		if err := cgi.Serve(http.HandlerFunc(webHandler)); err != nil {
			log.Printf("mirage.cgi: %v", err)
			return err
		}
		return nil
	}

	tlsConfig := tlsConfigFromEnvironment()
	if tlsConfig != nil {
		server := &http.Server{
			Addr:      webArgs.listen,
			TLSConfig: tlsConfig,
			Handler:   http.HandlerFunc(webHandler),
		}

		log.Printf("web server running on: https://%s", server.Addr)
		return server.ListenAndServeTLS("", "")
	} else {
		log.Printf("web server running on: %s", urlOfListenAddr(webArgs.listen))
		return http.ListenAndServe(webArgs.listen, http.HandlerFunc(webHandler))
	}
}

// urlOfListenAddr parses a given listen address into a formatted URL
func urlOfListenAddr(addr string) string {
	host, port, _ := net.SplitHostPort(addr)
	return fmt.Sprintf("http://%s", net.JoinHostPort(cmpx.Or(host, "127.0.0.1"), port))
}

// authorize returns the name of the user accessing the web UI after verifying
// whether the user has access to the web UI. The function will write the
// error to the provided http.ResponseWriter.
// Note: This is different from a tailscale user, and is typically the local
// user on the node.
func authorize(w http.ResponseWriter, r *http.Request) (string, error) {
	switch distro.Get() {
	case distro.Synology:
		user, err := synoAuthn()
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return "", err
		}
		if err := authorizeSynology(user); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return "", err
		}
		return user, nil
	case distro.QNAP:
		user, resp, err := qnapAuthn(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return "", err
		}
		if resp.IsAdmin == 0 {
			http.Error(w, err.Error(), http.StatusForbidden)
			return "", err
		}
		return user, nil
	}
	return "", nil
}

// authorizeSynology checks whether the provided user has access to the web UI
// by consulting the membership of the "administrators" group.
func authorizeSynology(name string) error {
	yes, err := groupmember.IsMemberOfGroup("administrators", name)
	if err != nil {
		return err
	}
	if !yes {
		return fmt.Errorf("not a member of administrators group")
	}
	return nil
}

type qnapAuthResponse struct {
	AuthPassed int    `xml:"authPassed"`
	IsAdmin    int    `xml:"isAdmin"`
	AuthSID    string `xml:"authSid"`
	ErrorValue int    `xml:"errorValue"`
}

func qnapAuthn(r *http.Request) (string, *qnapAuthResponse, error) {
	user, err := r.Cookie("NAS_USER")
	if err != nil {
		return "", nil, err
	}
	token, err := r.Cookie("qtoken")
	if err == nil {
		return qnapAuthnQtoken(r, user.Value, token.Value)
	}
	sid, err := r.Cookie("NAS_SID")
	if err == nil {
		return qnapAuthnSid(r, user.Value, sid.Value)
	}
	return "", nil, fmt.Errorf("not authenticated by any mechanism")
}

// qnapAuthnURL returns the auth URL to use by inferring where the UI is
// running based on the request URL. This is necessary because QNAP has so
// many options, see https://github.com/tailscale/tailscale/issues/7108
// and https://github.com/tailscale/tailscale/issues/6903
func qnapAuthnURL(requestUrl string, query url.Values) string {
	in, err := url.Parse(requestUrl)
	scheme := ""
	host := ""
	if err != nil || in.Scheme == "" {
		log.Printf("Cannot parse QNAP login URL %v", err)

		// try localhost and hope for the best
		scheme = "http"
		host = "localhost"
	} else {
		scheme = in.Scheme
		host = in.Host
	}

	u := url.URL{
		Scheme:   scheme,
		Host:     host,
		Path:     "/cgi-bin/authLogin.cgi",
		RawQuery: query.Encode(),
	}

	return u.String()
}

func qnapAuthnQtoken(r *http.Request, user, token string) (string, *qnapAuthResponse, error) {
	query := url.Values{
		"qtoken": []string{token},
		"user":   []string{user},
	}
	return qnapAuthnFinish(user, qnapAuthnURL(r.URL.String(), query))
}

func qnapAuthnSid(r *http.Request, user, sid string) (string, *qnapAuthResponse, error) {
	query := url.Values{
		"sid": []string{sid},
	}
	return qnapAuthnFinish(user, qnapAuthnURL(r.URL.String(), query))
}

func qnapAuthnFinish(user, url string) (string, *qnapAuthResponse, error) {
	// QNAP Force HTTPS mode uses a self-signed certificate. Even importing
	// the QNAP root CA isn't enough, the cert doesn't have a usable CN nor
	// SAN. See https://github.com/tailscale/tailscale/issues/6903
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Get(url)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	authResp := &qnapAuthResponse{}
	if err := xml.Unmarshal(out, authResp); err != nil {
		return "", nil, err
	}
	if authResp.AuthPassed == 0 {
		return "", nil, fmt.Errorf("not authenticated")
	}
	return user, authResp, nil
}

func synoAuthn() (string, error) {
	cmd := exec.Command("/usr/syno/synoman/webman/modules/authenticate.cgi")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("auth: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func authRedirect(w http.ResponseWriter, r *http.Request) bool {
	if distro.Get() == distro.Synology {
		return synoTokenRedirect(w, r)
	}
	return false
}

func synoTokenRedirect(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Syno-Token") != "" {
		return false
	}
	if r.URL.Query().Get("SynoToken") != "" {
		return false
	}
	if r.Method == "POST" && r.FormValue("SynoToken") != "" {
		return false
	}
	// We need a SynoToken for authenticate.cgi.
	// So we tell the client to get one.
	_, _ = fmt.Fprint(w, synoTokenRedirectHTML)
	return true
}

const synoTokenRedirectHTML = `<html><body>
Redirecting with session token...
<script>
var serverURL = window.location.protocol + "//" + window.location.host;
var req = new XMLHttpRequest();
req.overrideMimeType("application/json");
req.open("GET", serverURL + "/webman/login.cgi", true);
req.onload = function() {
	var jsonResponse = JSON.parse(req.responseText);
	var token = jsonResponse["SynoToken"];
	document.location.href = serverURL + "/webman/3rdparty/Mirage/?SynoToken=" + token;
};
req.send(null);
</script>
</body></html>
`

func webHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if authRedirect(w, r) {
		return
	}

	user, err := authorize(w, r)
	if err != nil {
		return
	}

	if r.URL.Path == "/redirect" || r.URL.Path == "/redirect/" {
		io.WriteString(w, authenticationRedirectHTML)
		return
	}

	st, err := localClient.StatusWithoutPeers(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	prefs, err := localClient.GetPrefs(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if r.Method == "POST" {
		defer r.Body.Close()
		var postData postedData
		type mi map[string]any
		if err := json.NewDecoder(r.Body).Decode(&postData); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(mi{"error": err.Error()})
			return
		}

		routes, err := calcAdvertiseRoutes(postData.AdvertiseRoutes, postData.AdvertiseExitNode)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(mi{"error": err.Error()})
			return
		}

		if postData.ServerCode != "" && postData.ServerCode != "NOUPDATE" && !strings.Contains(postData.ServerCode, "https://") && !strings.Contains(postData.ServerCode, "http://") {
			postData.ServerCode = "https://" + postData.ServerCode
		} else if postData.ServerCode == "" {
			postData.ServerCode = ipn.DefaultControlURL
		}

		mp := &ipn.MaskedPrefs{
			ControlURLSet:      postData.ServerCode != "NOUPDATE" && prefs.ControlURL != postData.ServerCode,
			AdvertiseRoutesSet: true,
			WantRunningSet:     true,
		}
		mp.Prefs.ControlURL = postData.ServerCode
		mp.Prefs.WantRunning = true
		mp.Prefs.AdvertiseRoutes = routes
		log.Printf("Doing edit: %v", mp.Pretty())

		if _, err := localClient.EditPrefs(ctx, mp); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(mi{"error": err.Error()})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		var reauth, logout bool
		if postData.Reauthenticate {
			reauth = true
		}
		if postData.ForceLogout {
			logout = true
		}
		log.Printf("mirageUp(reauth=%v, logout=%v) ...", reauth, logout)
		url, err := tailscaleUp(r.Context(), st, postData)
		log.Printf("mirageUp = (URL %v, %v)", url != "", err)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(mi{"error": err.Error()})
			return
		}
		if url != "" {
			json.NewEncoder(w).Encode(mi{"url": url})
		} else {
			io.WriteString(w, "{}")
		}
		return
	}

	profile := st.User[st.Self.UserID]
	deviceName := strings.Split(st.Self.DNSName, ".")[0]
	versionShort := strings.Split(st.Version, "-")[0]
	data := tmplData{
		SynologyUser: user,
		Profile:      profile,
		Status:       st.BackendState,
		DeviceName:   deviceName,
		LicensesURL:  licensesURL(),
		TUNMode:      st.TUN,
		IsSynology:   distro.Get() == distro.Synology || envknob.Bool("TS_FAKE_SYNOLOGY"),
		DSMVersion:   distro.DSMVersion(),
		IsUnraid:     distro.Get() == distro.Unraid,
		UnraidToken:  os.Getenv("UNRAID_CSRF_TOKEN"),
		IPNVersion:   versionShort,
	}
	exitNodeRouteV4 := netip.MustParsePrefix("0.0.0.0/0")
	exitNodeRouteV6 := netip.MustParsePrefix("::/0")
	for _, r := range prefs.AdvertiseRoutes {
		if r == exitNodeRouteV4 || r == exitNodeRouteV6 {
			data.AdvertiseExitNode = true
		} else {
			if data.AdvertiseRoutes != "" {
				data.AdvertiseRoutes += ","
			}
			data.AdvertiseRoutes += r.String()
		}
	}
	if len(st.TailscaleIPs) != 0 {
		data.IP = st.TailscaleIPs[0].String()
	}

	buf := new(bytes.Buffer)
	if err := tmpl.Execute(buf, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(buf.Bytes())
}

func tailscaleUp(ctx context.Context, st *ipnstate.Status, postData postedData) (authURL string, retErr error) {
	if postData.ForceLogout {
		if err := localClient.Logout(ctx); err != nil {
			return "", fmt.Errorf("Logout error: %w", err)
		}
		return "", nil
	}

	origAuthURL := st.AuthURL
	isRunning := st.BackendState == ipn.Running.String()

	forceReauth := postData.Reauthenticate
	if !forceReauth {
		if origAuthURL != "" {
			return origAuthURL, nil
		}
		if isRunning {
			return "", nil
		}
	}

	// printAuthURL reports whether we should print out the
	// provided auth URL from an IPN notify.
	printAuthURL := func(url string) bool {
		return url != origAuthURL
	}

	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	watcher, err := localClient.WatchIPNBus(watchCtx, 0)
	if err != nil {
		return "", err
	}
	defer watcher.Close()

	go func() {
		if !isRunning {
			localClient.Start(ctx, ipn.Options{})
		}
		if forceReauth {
			localClient.StartLoginInteractive(ctx)
		}
	}()

	for {
		n, err := watcher.Next()
		if err != nil {
			return "", err
		}
		if n.ErrMessage != nil {
			msg := *n.ErrMessage
			return "", fmt.Errorf("backend error: %v", msg)
		}
		if url := n.BrowseToURL; url != nil && printAuthURL(*url) {
			return *url, nil
		}
	}
}
