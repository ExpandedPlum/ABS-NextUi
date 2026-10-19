package service

// Phone-assisted login. Typing a server address, password or API key on the
// handheld's on-screen keyboard is slow, so the daemon can serve a one-time
// login page on the local network. The UI shows its address as a QR code;
// the form is filled in on a phone and the daemon logs in with it.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

type PhoneLogin struct {
	Active  bool   `json:"active"`
	URL     string `json:"url,omitempty"`
	QR      string `json:"qr,omitempty"` // PNG path for the UI
	Message string `json:"message,omitempty"`
}

type phoneServer struct {
	srv   *http.Server
	token string
	state PhoneLogin
	timer *time.Timer
}

// lanIP picks the address other devices on the network can reach.
func lanIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	best := ""
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			if strings.HasPrefix(ifc.Name, "wlan") {
				return ipn.IP.String()
			}
			if best == "" {
				best = ipn.IP.String()
			}
		}
	}
	return best
}

func (s *Service) phoneStatus() *PhoneLogin {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phone == nil {
		return nil
	}
	st := s.phone.state
	return &st
}

func (s *Service) StartPhoneLogin() (*PhoneLogin, error) {
	s.mu.Lock()
	if s.phone != nil && s.phone.state.Active {
		st := s.phone.state
		s.mu.Unlock()
		return &st, nil
	}
	s.mu.Unlock()

	ip := lanIP()
	if ip == "" {
		return nil, errors.New("connect to Wi-Fi first")
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])

	ln, err := net.Listen("tcp", ":8737")
	if err != nil {
		ln, err = net.Listen("tcp", ":0")
		if err != nil {
			return nil, err
		}
	}
	port := ln.Addr().(*net.TCPAddr).Port
	url := fmt.Sprintf("http://%s:%d/%s", ip, port, token)
	qrPath := filepath.Join(s.St.CacheDir(), "phone-login.png")
	if err := qrcode.WriteFile(url, qrcode.Medium, 512, qrPath); err != nil {
		ln.Close()
		return nil, err
	}

	ps := &phoneServer{token: token, state: PhoneLogin{Active: true, URL: url, QR: qrPath}}
	mux := http.NewServeMux()
	mux.HandleFunc("/"+token, s.servePhoneForm)
	ps.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ps.timer = time.AfterFunc(10*time.Minute, s.StopPhoneLogin)

	s.mu.Lock()
	s.phone = ps
	s.mu.Unlock()
	go func() {
		if err := ps.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("phone login server: %v", err)
		}
	}()
	log.Printf("phone login available at %s", strings.Replace(url, token, "<token>", 1))
	st := ps.state
	return &st, nil
}

func (s *Service) StopPhoneLogin() {
	s.mu.Lock()
	ps := s.phone
	s.phone = nil
	s.mu.Unlock()
	if ps == nil {
		return
	}
	ps.timer.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = ps.srv.Shutdown(ctx)
}

func (s *Service) setPhoneMessage(msg string) {
	s.mu.Lock()
	if s.phone != nil {
		s.phone.state.Message = msg
	}
	s.mu.Unlock()
}

var phoneTmpl = template.Must(template.New("p").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Audiobookshelf on your handheld</title>
<style>
body{font:16px system-ui,sans-serif;margin:0;padding:24px;background:#111;color:#eee}
main{max-width:420px;margin:auto}h1{font-size:1.3em}
label{display:block;margin:14px 0 4px;color:#aaa;font-size:.9em}
input,textarea{width:100%;box-sizing:border-box;padding:10px;border-radius:8px;border:1px solid #444;background:#1d1d1d;color:#eee;font-size:16px}
button{margin-top:20px;width:100%;padding:12px;border:0;border-radius:8px;background:#9b2257;color:#fff;font-size:17px}
.tabs{display:flex;gap:8px;margin-top:16px}.tabs label{flex:1;text-align:center;border:1px solid #444;border-radius:8px;padding:8px;margin:0;color:#eee}
.tabs input{display:none}.tabs input:checked+span{color:#fff;font-weight:600}
.msg{padding:12px;border-radius:8px;background:#1d3b24;margin-bottom:12px}.err{background:#4a1d1d}
small{color:#888}
</style></head><body><main>
<h1>Sign in to Audiobookshelf</h1>
{{if .Message}}<div class="msg {{if .Error}}err{{end}}">{{.Message}}</div>{{end}}
{{if not .Done}}
<form method="post">
<label>Server address</label>
<input name="server" value="{{.Server}}" placeholder="192.168.1.20:13378 or https://abs.example.com" autocapitalize="off" autocorrect="off" required>
<div class="tabs">
<label><input type="radio" name="method" value="password" {{if ne .Method "apikey"}}checked{{end}} onchange="sw()"><span>Password</span></label>
<label><input type="radio" name="method" value="apikey" {{if eq .Method "apikey"}}checked{{end}} onchange="sw()"><span>API key</span></label>
</div>
<div id="pw">
<label>Username</label><input name="username" value="{{.Username}}" autocapitalize="off" autocorrect="off">
<label>Password</label><input name="password" type="password">
</div>
<div id="key">
<label>API key</label><textarea name="apikey" rows="4" autocapitalize="off" autocorrect="off"></textarea>
<small>Create one in Audiobookshelf under Settings &rarr; API Keys. Use this if your account signs in with SSO.</small>
</div>
<button>Sign in</button>
</form>
<script>function sw(){var k=document.querySelector('input[value=apikey]').checked;document.getElementById('pw').style.display=k?'none':'';document.getElementById('key').style.display=k?'':'none'}sw()</script>
{{end}}
</main></body></html>`))

func (s *Service) servePhoneForm(w http.ResponseWriter, r *http.Request) {
	cr := s.Client.Credentials()
	data := struct {
		Server, Username, Method, Message string
		Error, Done                       bool
	}{Server: cr.Server, Username: cr.Username}
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		data.Server = r.FormValue("server")
		data.Username = r.FormValue("username")
		data.Method = r.FormValue("method")
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		var err error
		if data.Method == "apikey" {
			err = s.Client.LoginAPIKey(ctx, data.Server, r.FormValue("apikey"))
		} else {
			err = s.Client.Login(ctx, data.Server, data.Username, r.FormValue("password"))
		}
		cancel()
		s.noteNetErr(err)
		if err != nil {
			data.Error, data.Message = true, "Couldn't sign in: "+userError(err)
			s.setPhoneMessage("Sign-in failed, try again on your phone")
		} else {
			s.afterLogin()
			data.Done = true
			data.Message = "Signed in as " + s.Client.Credentials().Username + ". You can put your phone away."
			s.setPhoneMessage("Signed in")
			go func() {
				time.Sleep(3 * time.Second)
				s.StopPhoneLogin()
			}()
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = phoneTmpl.Execute(w, data)
}

// afterLogin resets per-account state.
func (s *Service) afterLogin() {
	set := s.St.GetSettings()
	set.LibraryID = ""
	_ = s.St.SaveSettings(set)
	s.invalidateProgress()
}
