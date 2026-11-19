package api

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The sign-in and consent pages of the OAuth server are rendered here, by
// the control plane itself, in English or Turkish. They use no script and no
// external resource; the only style sheet is inline and allowed by a
// per-response nonce.

const localeCookie = "la_locale"

// pageStrings are the texts of one language.
type pageStrings struct {
	Lang, Title string

	SignInHeading, SignInIntro  string
	Email, Password, TOTP       string
	TOTPHint, SignIn            string
	ConsentHeading, ConsentLead string
	SignedInAs, NotYou          string
	ReturnsTo                   string
	UnverifiedName              string
	VerifiedDomain              string
	LoopbackWarning             string
	HostMismatch                string
	WillBeAble, ScopeRead       string
	ScopeAdmin, ScopeAdminOpt   string
	ActsAsYou, NoOrgs           string
	Roles                       map[string]string
	Lifetime                    string
	Approve, Deny               string
	Revocable                   string
	ErrorHeading, ErrorLead     string
	ErrForm, ErrCredentials     string
	ErrTOTPRequired, ErrTOTP    string
	ErrRateLimited, ErrInternal string
	OtherLanguage, OtherLangTag string
}

var pageLocales = map[string]*pageStrings{
	"en": {
		Lang: "en", Title: "izlek · Connect an application",
		SignInHeading: "Sign in to izlek",
		SignInIntro:   "An application is asking for access to your izlek account. Sign in to see what it is asking for; nothing is shared until you approve.",
		Email:         "E-mail", Password: "Password", TOTP: "Verification code",
		TOTPHint: "Only if two-step verification is enabled for your account.", SignIn: "Sign in",
		ConsentHeading: "Connect an application",
		ConsentLead:    "wants to access izlek on your behalf.",
		SignedInAs:     "You are signed in as", NotYou: "Use a different account",
		ReturnsTo:       "After you approve, access is handed to",
		UnverifiedName:  "This application registered itself and chose its own name; izlek has not verified who runs it. The address above is the only reliable hint. Approve only if you started this connection yourself, just now.",
		VerifiedDomain:  "The application's details were read from",
		LoopbackWarning: "That address is on your own computer. Any program running there could receive the access, so approve only if you just started this connection from a tool on this machine.",
		HostMismatch:    "Note: the application is described by one site but sends the access to another.",
		WillBeAble:      "If you approve, it will be able to:",
		ScopeRead:       "View your projects, write tokens and feature flags, and run analytics queries — including raw SQL over events and people.",
		ScopeAdmin:      "Create and change projects, write tokens, secret keys and feature flags.",
		ScopeAdminOpt:   "Allow changes (untick to connect read-only)",
		ActsAsYou:       "It acts as you, with your role in each organisation:",
		NoOrgs:          "Your account does not belong to any organisation, so there is nothing it could reach.",
		Roles:           map[string]string{"owner": "owner", "admin": "admin", "member": "member", "viewer": "viewer"},
		Lifetime:        "It cannot close projects, delete data, manage members or change your password. Access ends after 180 days, when you change your password, or when you revoke it.",
		Approve:         "Approve", Deny: "Deny",
		Revocable:    "You can revoke access at any time from your izlek account settings.",
		ErrorHeading: "This connection cannot continue",
		ErrorLead:    "The request from the application is not valid, so izlek stopped here instead of sending you back to it. Close this page and start the connection again from the application.",
		ErrForm:      "This page has expired. Reload it and try again.", ErrCredentials: "The e-mail address or password is not correct.",
		ErrTOTPRequired: "Enter the verification code from your authenticator app.", ErrTOTP: "The verification code is not correct.",
		ErrRateLimited: "Too many attempts. Wait a minute and try again.", ErrInternal: "Something went wrong. Try again.",
		OtherLanguage: "Türkçe", OtherLangTag: "tr",
	},
	"tr": {
		Lang: "tr", Title: "izlek · Uygulama bağla",
		SignInHeading: "izlek'e giriş yapın",
		SignInIntro:   "Bir uygulama izlek hesabınıza erişmek istiyor. Ne istediğini görmek için giriş yapın; siz onaylamadan hiçbir şey paylaşılmaz.",
		Email:         "E-posta", Password: "Parola", TOTP: "Doğrulama kodu",
		TOTPHint: "Yalnızca hesabınızda iki adımlı doğrulama açıksa.", SignIn: "Giriş yap",
		ConsentHeading: "Uygulama bağla",
		ConsentLead:    "sizin adınıza izlek'e erişmek istiyor.",
		SignedInAs:     "Giriş yapılan hesap:", NotYou: "Başka bir hesap kullan",
		ReturnsTo:       "Onaylarsanız erişim şu adrese teslim edilir:",
		UnverifiedName:  "Bu uygulama kendini kaydetti ve adını kendisi seçti; kimin işlettiğini izlek doğrulamadı. Güvenilir tek ipucu yukarıdaki adrestir. Yalnızca bu bağlantıyı az önce kendiniz başlattıysanız onaylayın.",
		VerifiedDomain:  "Uygulamanın bilgileri şu adresten okundu:",
		LoopbackWarning: "Bu adres kendi bilgisayarınızdadır. Orada çalışan herhangi bir program erişimi alabilir; yalnızca bu bağlantıyı az önce bu makinedeki bir araçtan başlattıysanız onaylayın.",
		HostMismatch:    "Not: uygulamayı bir site tanımlıyor, ancak erişim başka bir siteye gönderiliyor.",
		WillBeAble:      "Onaylarsanız şunları yapabilecek:",
		ScopeRead:       "Projelerinizi, yazma token'larını ve feature flag'leri görmek; analitik sorgular çalıştırmak (event ve kişi verisi üzerinde ham SQL dahil).",
		ScopeAdmin:      "Proje, yazma token'ı, gizli anahtar ve feature flag oluşturmak ve değiştirmek.",
		ScopeAdminOpt:   "Değişikliklere izin ver (salt okunur bağlamak için işareti kaldırın)",
		ActsAsYou:       "Sizin adınıza, her organizasyondaki rolünüzle hareket eder:",
		NoOrgs:          "Hesabınız hiçbir organizasyona üye değil; uygulamanın ulaşabileceği bir şey yok.",
		Roles:           map[string]string{"owner": "sahip", "admin": "yönetici", "member": "üye", "viewer": "izleyici"},
		Lifetime:        "Proje kapatamaz, veri silemez, üyeleri yönetemez, parolanızı değiştiremez. Erişim 180 gün sonra, parolanızı değiştirdiğinizde ya da siz iptal ettiğinizde sona erer.",
		Approve:         "Onayla", Deny: "Reddet",
		Revocable:    "Erişimi izlek hesap ayarlarınızdan istediğiniz zaman iptal edebilirsiniz.",
		ErrorHeading: "Bu bağlantı sürdürülemiyor",
		ErrorLead:    "Uygulamadan gelen istek geçerli değil; izlek sizi uygulamaya geri göndermek yerine burada durdu. Bu sayfayı kapatıp bağlantıyı uygulamadan yeniden başlatın.",
		ErrForm:      "Bu sayfanın süresi doldu. Yenileyip yeniden deneyin.", ErrCredentials: "E-posta adresi ya da parola hatalı.",
		ErrTOTPRequired: "Doğrulama uygulamanızdaki kodu girin.", ErrTOTP: "Doğrulama kodu hatalı.",
		ErrRateLimited: "Çok fazla deneme yapıldı. Bir dakika bekleyip yeniden deneyin.", ErrInternal: "Bir sorun oluştu. Yeniden deneyin.",
		OtherLanguage: "English", OtherLangTag: "en",
	},
}

// pickPageLocale chooses the language of a page: an explicit ui_locales
// parameter (OpenID Connect's name for it), then the la_locale cookie, then
// Accept-Language; English when nothing asks for Turkish.
func pickPageLocale(r *http.Request, uiLocales string) *pageStrings {
	for _, tag := range strings.Fields(uiLocales) {
		if l := localeFor(tag); l != nil {
			return l
		}
	}
	if ck, err := r.Cookie(localeCookie); err == nil {
		if l := localeFor(ck.Value); l != nil {
			return l
		}
	}
	// The first language the browser lists that the pages exist in.
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		if l := localeFor(tag); l != nil {
			return l
		}
	}
	return pageLocales["en"]
}

func localeFor(tag string) *pageStrings {
	tag = strings.ToLower(strings.TrimSpace(tag))
	primary, _, _ := strings.Cut(tag, "-")
	return pageLocales[primary]
}

type orgLine struct{ Name, Role string }

// pageData is everything a page shows.
type pageData struct {
	T     *pageStrings
	Nonce string
	Kind  string // "login" | "consent" | "error"
	// Query is the authorization request, carried through the forms.
	Query, CSRF  string
	LanguageLink string

	Notice string // a problem to show above the form

	Email string // login: pre-filled; consent: the signed-in account

	ClientName, RedirectHost string
	MetadataHost             string // cimd: host of the metadata document
	Unverified, Loopback     bool
	HostMismatch             bool
	WantsAdmin               bool
	Orgs                     []orgLine

	ErrorDetail string
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="{{.T.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.T.Title}}</title>
<style nonce="{{.Nonce}}">
:root { color-scheme: light dark; --bg: #f6f7f9; --card: #fff; --fg: #15181d; --muted: #5d6673; --line: #d9dde3; --accent: #1f5fd1; --accent-fg: #fff; --warn-bg: #fff6e0; --warn-fg: #6b4a00; --err-bg: #fdecec; --err-fg: #8c1d18; }
@media (prefers-color-scheme: dark) { :root { --bg: #111418; --card: #1a1e24; --fg: #e8ebef; --muted: #9aa4b2; --line: #2e343d; --accent: #6ea2ff; --accent-fg: #0b1220; --warn-bg: #3a2f10; --warn-fg: #f3d58a; --err-bg: #3d1a18; --err-fg: #ffb4ab; } }
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--fg); font: 16px/1.5 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; }
main { max-width: 30rem; margin: 0 auto; padding: 2rem 1rem 3rem; }
.brand { font-weight: 700; letter-spacing: .02em; margin: 0 0 1rem; color: var(--muted); }
.card { background: var(--card); border: 1px solid var(--line); border-radius: 12px; padding: 1.5rem; }
h1 { font-size: 1.35rem; line-height: 1.25; margin: 0 0 .75rem; }
p { margin: 0 0 1rem; }
.muted { color: var(--muted); font-size: .9rem; }
.client { font-weight: 700; overflow-wrap: anywhere; }
.host { display: block; font: 600 1rem/1.4 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; background: var(--bg); border: 1px solid var(--line); border-radius: 8px; padding: .5rem .75rem; margin: .25rem 0 1rem; overflow-wrap: anywhere; }
.note { border-radius: 8px; padding: .75rem; margin: 0 0 1rem; font-size: .9rem; }
.warn { background: var(--warn-bg); color: var(--warn-fg); }
.err { background: var(--err-bg); color: var(--err-fg); }
ul { margin: 0 0 1rem; padding-left: 1.25rem; }
li { margin: 0 0 .4rem; }
label { display: block; font-weight: 600; margin: 0 0 .25rem; }
label.check { display: flex; gap: .5rem; align-items: flex-start; font-weight: 400; margin: 0 0 1rem; }
input[type=email], input[type=password], input[type=text] { width: 100%; font: inherit; color: inherit; background: var(--bg); border: 1px solid var(--line); border-radius: 8px; padding: .6rem .75rem; margin: 0 0 1rem; }
.hint { display: block; font-weight: 400; }
.row { display: flex; gap: .75rem; flex-wrap: wrap; }
button { font: inherit; font-weight: 600; border-radius: 8px; padding: .65rem 1.1rem; border: 1px solid var(--line); background: var(--card); color: var(--fg); cursor: pointer; }
button.primary { background: var(--accent); border-color: var(--accent); color: var(--accent-fg); }
button.link { border: 0; background: none; padding: 0; color: var(--accent); font-weight: 400; text-decoration: underline; }
:focus-visible { outline: 3px solid var(--accent); outline-offset: 2px; }
.account { display: flex; gap: .5rem; flex-wrap: wrap; align-items: baseline; margin: 0 0 1rem; }
footer { margin-top: 1rem; display: flex; justify-content: space-between; gap: 1rem; flex-wrap: wrap; }
a { color: var(--accent); }
</style>
</head>
<body>
<main>
<p class="brand">izlek</p>
<div class="card">
{{if eq .Kind "login"}}
<h1>{{.T.SignInHeading}}</h1>
<p class="muted">{{.T.SignInIntro}}</p>
{{if .Notice}}<p class="note err" role="alert">{{.Notice}}</p>{{end}}
<form method="post" action="/oauth/authorize">
<input type="hidden" name="q" value="{{.Query}}">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<label for="email">{{.T.Email}}</label>
<input id="email" name="email" type="email" autocomplete="username" required value="{{.Email}}">
<label for="password">{{.T.Password}}</label>
<input id="password" name="password" type="password" autocomplete="current-password" required>
<label for="totp">{{.T.TOTP}} <span class="hint muted">{{.T.TOTPHint}}</span></label>
<input id="totp" name="totp" type="text" inputmode="numeric" autocomplete="one-time-code" maxlength="8">
<button class="primary" type="submit" name="action" value="login">{{.T.SignIn}}</button>
</form>
{{else if eq .Kind "consent"}}
<h1>{{.T.ConsentHeading}}</h1>
<p><span class="client">{{.ClientName}}</span> {{.T.ConsentLead}}</p>
{{if .Notice}}<p class="note err" role="alert">{{.Notice}}</p>{{end}}
<p class="muted">{{.T.ReturnsTo}}</p>
<span class="host">{{.RedirectHost}}</span>
{{if .MetadataHost}}<p class="muted">{{.T.VerifiedDomain}} <strong>{{.MetadataHost}}</strong></p>{{end}}
{{if .HostMismatch}}<p class="note warn">{{.T.HostMismatch}}</p>{{end}}
{{if .Loopback}}<p class="note warn">{{.T.LoopbackWarning}}</p>{{end}}
{{if .Unverified}}<p class="note warn">{{.T.UnverifiedName}}</p>{{end}}
<form method="post" action="/oauth/authorize">
<input type="hidden" name="q" value="{{.Query}}">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<div class="account"><span>{{.T.SignedInAs}} <strong>{{.Email}}</strong></span>
<button class="link" type="submit" name="action" value="logout" formnovalidate>{{.T.NotYou}}</button></div>
<p>{{.T.WillBeAble}}</p>
<ul>
<li>{{.T.ScopeRead}}</li>
{{if .WantsAdmin}}<li>{{.T.ScopeAdmin}}</li>{{end}}
</ul>
{{if .WantsAdmin}}<label class="check"><input type="checkbox" name="grant_admin" value="1" checked> <span>{{.T.ScopeAdminOpt}}</span></label>{{end}}
{{if .Orgs}}<p>{{.T.ActsAsYou}}</p>
<ul>{{range .Orgs}}<li><strong>{{.Name}}</strong> — {{.Role}}</li>{{end}}</ul>
{{else}}<p class="note warn">{{.T.NoOrgs}}</p>{{end}}
<p class="muted">{{.T.Lifetime}}</p>
<div class="row">
<button class="primary" type="submit" name="action" value="approve">{{.T.Approve}}</button>
<button type="submit" name="action" value="deny">{{.T.Deny}}</button>
</div>
</form>
{{else}}
<h1>{{.T.ErrorHeading}}</h1>
<p>{{.T.ErrorLead}}</p>
{{if .ErrorDetail}}<p class="note err" lang="en">{{.ErrorDetail}}</p>{{end}}
{{end}}
</div>
<footer class="muted">
<span>{{if eq .Kind "consent"}}{{.T.Revocable}}{{end}}</span>
{{if .LanguageLink}}<a href="{{.LanguageLink}}" lang="{{.T.OtherLangTag}}" hreflang="{{.T.OtherLangTag}}">{{.T.OtherLanguage}}</a>{{end}}
</footer>
</main>
</body>
</html>
`))

// renderPage writes one of the OAuth pages. formTargets are the origins a
// form on the page may end up at after the server's redirect: browsers apply
// form-action to the whole redirect chain of a form submission, so the
// client's redirect origin has to be named for the Approve button to work.
func (s *Server) renderPage(w http.ResponseWriter, status int, data pageData, formTargets ...string) {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	data.Nonce = base64.RawStdEncoding.EncodeToString(raw)
	formAction := "'self'"
	for _, origin := range formTargets {
		if origin != "" {
			formAction += " " + origin
		}
	}
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'nonce-"+data.Nonce+"'; form-action "+formAction+"; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("Content-Type", "text/html; charset=utf-8")
	var buf bytes.Buffer
	if err := pageTemplate.Execute(&buf, data); err != nil {
		s.log.Error("cannot render oauth page", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// originOf returns scheme://host of a URL, for a CSP source list.
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || strings.ContainsAny(u.Host, " ;,'\"") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
