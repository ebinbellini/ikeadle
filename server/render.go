package server

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sync"
)

// templateFS holds the HTML templates compiled into the binary, so a deployed
// server always renders exactly the markup it was built with. The templates
// are only static HTML; everything a page needs is passed in as data.
//
//go:embed templates
var templateFS embed.FS

// Page names, used both as template set keys and as file names under
// templates/pages.
const (
	dailyPage = "daily"
	slumpPage = "slump"
	stridPage = "strid"
	statsPage = "statistik"
)

// Mode is one entry in the mode picker in the navbar.
type Mode struct {
	Href  string
	Label string
	Icon  string
}

// modes is shared by every page so the navbar stays identical everywhere. The
// order is the order shown to the player. Statistics is not a game mode, but it
// lives in the same menu so every page can reach it.
var modes = []Mode{
	{Href: "/", Label: "Dagens vara", Icon: "/icons/calendar.svg"},
	{Href: "/slump", Label: "Jag har tur", Icon: "/icons/cart.svg"},
	{Href: "/strid", Label: "Kund mot kund", Icon: "/icons/fight.svg"},
	{Href: "/statistik", Label: "Statistik", Icon: "/icons/statistik.svg"},
}

// pageData is what the shared layout and the navbar get from every page.
type pageData struct {
	// Title is the document title, ActiveLink the href of the mode to
	// highlight in the navbar.
	Title      string
	ActiveLink string
	Modes      []Mode
}

// productCard is the product a game page shows. The price is not part of it:
// that is what the player guesses.
type productCard struct {
	HasProduct  bool
	Name        string
	Description string
	Amount      string
	Category    string
	ImageURL    string
}

// gamePageData feeds the game board that both playable modes share. Only the
// intro and the data attributes differ between them.
type gamePageData struct {
	pageData
	Product    productCard
	MaxGuesses int
	Tolerance  string
	// Date is set on the daily page (the day the product belongs to) and
	// ProductID on the slump page (the product being played).
	Date      string
	ProductID string
}

// stridPageData feeds the "Kund mot kund" page. The lobby itself is rendered
// by the client from the event stream, so the server only needs to say which
// code was opened and what state that code is in.
type stridPageData struct {
	pageData
	Product productCard

	Code      string
	CodeState string

	MinPlayers   int
	MaxPlayers   int
	Rounds       int
	GuessSeconds int
}

// pageTemplates parses every page once. A page is the shared layout, navbar and
// partials merged with one file from templates/pages: having a template set per
// page means the "content" block of one page cannot leak into another.
var pageTemplates = sync.OnceValues(func() (map[string]*template.Template, error) {
	shared := []string{
		"templates/layout.html",
		"templates/navbar.html",
		"templates/product-card.html",
		"templates/guess-board.html",
	}

	pages := make(map[string]*template.Template, 4)
	for _, page := range []string{dailyPage, slumpPage, stridPage, statsPage} {
		files := append([]string{"templates/pages/" + page + ".html"}, shared...)
		tmpl, err := template.New(page).ParseFS(templateFS, files...)
		if err != nil {
			return nil, fmt.Errorf("parse %q page: %w", page, err)
		}
		pages[page] = tmpl
	}

	return pages, nil
})

// LoadTemplates parses the templates up front so a broken template is reported
// at startup instead of on the first request.
func LoadTemplates() error {
	_, err := pageTemplates()
	return err
}

// renderPage renders a page into a buffer first, so a template error leaves the
// response untouched instead of sending half a document.
func renderPage(w http.ResponseWriter, page string, data any) {
	templates, err := pageTemplates()
	if err != nil {
		log.Printf("Could not load template %q: %v", page, err)
		http.Error(w, "kunde inte rendera sidan", http.StatusInternalServerError)
		return
	}

	tmpl, ok := templates[page]
	if !ok {
		log.Printf("Unknown page %q", page)
		http.Error(w, "kunde inte rendera sidan", http.StatusInternalServerError)
		return
	}

	var body bytes.Buffer
	if err := tmpl.ExecuteTemplate(&body, "layout.html", data); err != nil {
		log.Printf("Could not render %q: %v", page, err)
		http.Error(w, "kunde inte rendera sidan", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := body.WriteTo(w); err != nil {
		log.Printf("Could not write %q response: %v", page, err)
	}
}

// productCardFrom turns a product into the card a game page renders.
func productCardFrom(product Product, ok bool) productCard {
	if !ok {
		return productCard{}
	}

	return productCard{
		HasProduct:  true,
		Name:        product.Name,
		Description: product.Description,
		Amount:      product.Amount,
		Category:    product.CategoryPath,
		ImageURL:    product.ImageURL,
	}
}

// DailyPage renders the daily game with the product of the day already in the
// markup, so the player sees the product without waiting for a fetch.
func DailyPage(w http.ResponseWriter, r *http.Request) {
	product, ok := CurrentDailyProduct()

	data := gamePageData{
		pageData: pageData{
			Title:      "IKEAdle - Dagens vara",
			ActiveLink: "/",
			Modes:      modes,
		},
		Product:    productCardFrom(product, ok),
		MaxGuesses: MaxGuesses,
		Tolerance:  toleranceText(),
	}
	if ok {
		data.Date = todayKey()
	}

	renderPage(w, dailyPage, data)
}

// StridPage renders the lobby shell for "Kund mot kund". The page works both
// as a landing page (/strid) and as an invite page (/strid/CODE); the client
// asks the API whether the code is open and whether this browser already has a
// seat in the lobby.
func StridPage(w http.ResponseWriter, r *http.Request) {
	code := ""
	codeState := ""

	if raw := r.PathValue("code"); raw != "" {
		normalized, ok := normalizeStridCode(raw)
		if !ok {
			http.Redirect(w, r, "/strid", http.StatusSeeOther)
			return
		}
		code = normalized
		codeState = stridLobbies.codeState(code)
	}

	renderPage(w, stridPage, stridPageData{
		pageData: pageData{
			Title:      "IKEAdle - Kund mot kund",
			ActiveLink: "/strid",
			Modes:      modes,
		},
		Code:         code,
		CodeState:    codeState,
		MinPlayers:   stridMinPlayers,
		MaxPlayers:   stridMaxPlayers,
		Rounds:       stridRounds,
		GuessSeconds: stridGuessSeconds,
	})
}
