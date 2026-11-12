package server

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

type Product struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Description is IKEA's type and design, e.g. "Bokhylla, vit".
	Description string  `json:"description,omitempty"`
	URL         string  `json:"url"`
	ImageURL    string  `json:"image_url,omitempty"`
	Price       float64 `json:"price"`
	PriceUnit   string  `json:"price_unit"`
	// Amount is IKEA's measurement text, e.g. "80x28x202 cm".
	Amount string `json:"amount,omitempty"`
	// CategoryPath is IKEA's own breadcrumb, e.g.
	// "Förvaring / Hyllor & bokhyllor / Bokhyllor".
	CategoryPath string    `json:"category_path"`
	ScrapedAt    time.Time `json:"scraped_at"`
}

type Category struct {
	Slugs    []string  `json:"slugs"`
	Names    []string  `json:"names"`
	URL      string    `json:"url"`
	Products []Product `json:"products,omitempty"`
}

// CategoryMap holds every scraped category keyed by its category id (e.g.
// "st001" for Förvaring). The scraper replaces it through SetCategoryMap.
var CategoryMap = make(map[string]Category)

var categoryMu sync.RWMutex

// SetCategoryMap replaces the scraped categories and rebuilds the flat product
// pool that the product of the day is picked from.
func SetCategoryMap(categories map[string]Category) {
	categoryMu.Lock()
	CategoryMap = categories
	categoryMu.Unlock()

	productCount := 0
	for _, category := range categories {
		productCount += len(category.Products)
	}

	list := make([]Product, 0, productCount)
	for _, category := range categories {
		list = append(list, category.Products...)
	}

	SetProducts(list)
}

func Run() {
	if err := LoadTemplates(); err != nil {
		log.Fatalf("Error loading templates: %v", err)
	}

	stridLobbies.startJanitor()

	mux := newMux()

	listenAddr := os.Getenv("ADDR")
	if listenAddr == "" {
		listenAddr = ":2760"
	}
	fmt.Println("Starting IKEAdle on", listenAddr)
	err := http.ListenAndServe(listenAddr, mux)
	if err != nil {
		log.Fatalf("Error starting server: %v", err)
	}
}

// newMux registers every page, API and static route. It is separate from Run so
// tests can exercise the same routing without starting a listener.
func newMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Pages rendered from go templates
	mux.HandleFunc("GET /{$}", DailyPage)
	mux.HandleFunc("GET /slump", SlumpPage)
	mux.HandleFunc("GET /strid", StridPage)
	mux.HandleFunc("GET /strid/{code}", StridPage)
	mux.HandleFunc("GET /statistik", StatsPage)

	// Game API
	mux.HandleFunc("/api/daily", DailyHandler)
	mux.HandleFunc("/api/daily/guess", GuessHandler)
	mux.HandleFunc("/api/daily/answer", AnswerHandler)

	// Slump API
	mux.HandleFunc("/api/slump/product", SlumpProductHandler)
	mux.HandleFunc("/api/slump/guess", SlumpGuessHandler)
	mux.HandleFunc("/api/slump/answer", SlumpAnswerHandler)

	// Kund mot kund API
	mux.HandleFunc("/api/strid/lobbies", StridCreateHandler)
	mux.HandleFunc("/api/strid/join", StridJoinHandler)
	mux.HandleFunc("/api/strid/ready", StridReadyHandler)
	mux.HandleFunc("/api/strid/start", StridStartHandler)
	mux.HandleFunc("/api/strid/again", StridAgainHandler)
	mux.HandleFunc("/api/strid/guess", StridGuessHandler)
	mux.HandleFunc("/api/strid/state", StridStateHandler)
	mux.HandleFunc("/api/strid/events", StridEventsHandler)

	devMode := os.Getenv("DEV_MODE")
	if devMode == "true" {
		// Serve static files in development mode. The page routes above are
		// more specific, so they still win over this catch-all.
		staticFiles := http.FileServer(http.Dir("web_static"))
		mux.Handle("/", staticFiles)
	}

	return mux
}
