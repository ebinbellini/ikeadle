package server

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	// dailyStateFile keeps the product of the day across restarts so that the
	// answer does not change when the server restarts during the day.
	dailyStateFile  = "/tmp/ikeadle_daily.json"
	dailyDateLayout = "2006-01-02"
)

// DailyProduct is the product chosen for one specific day.
type DailyProduct struct {
	Date    string  `json:"date"`
	Product Product `json:"product"`
}

var (
	productsMu sync.RWMutex
	products   []Product
	// productIndex makes a product playable by id, which is what the slump mode
	// needs to score a guess against the item the player started with.
	productIndex = make(map[string]Product)

	dailyMu   sync.RWMutex
	dailyData *DailyProduct
)

// publicProduct is the part of a Product the frontend may see. The price is
// deliberately left out: that is what the player has to guess.
type publicProduct struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ImageURL    string `json:"image_url,omitempty"`
	Amount      string `json:"amount,omitempty"`
	PriceUnit   string `json:"price_unit,omitempty"`
	Category    string `json:"category,omitempty"`
}

type dailyResponse struct {
	Date    string        `json:"date"`
	Product publicProduct `json:"product"`
}

type guessRequest struct {
	Guess float64 `json:"guess"`
}

type guessResponse struct {
	// Result is "low", "high" or "correct" (seen from the guess' perspective).
	Result string `json:"result"`
	// Warmth is "correct", "warm" or "cold" and describes how close the guess
	// was: within 50 öre counts as correct, within 5 kr or 20% as warm.
	Warmth string  `json:"warmth"`
	Guess  float64 `json:"guess"`
}

type answerResponse struct {
	Name      string  `json:"name"`
	Amount    string  `json:"amount,omitempty"`
	Price     float64 `json:"price"`
	PriceUnit string  `json:"price_unit,omitempty"`
}

// playableProduct reports whether a product has enough information to be
// shown to the player.
func playableProduct(p Product) bool {
	return p.ID != "" && p.Name != "" && p.Price > 0 && p.ImageURL != ""
}

// SetProducts replaces the flat product pool that the daily product is picked
// from. Products that cannot be played are dropped, as are duplicates, since
// the same product can be listed in several categories.
func SetProducts(list []Product) {
	pool := make([]Product, 0, len(list))
	seen := make(map[string]bool, len(list))
	unplayable := 0

	for _, product := range list {
		if !playableProduct(product) {
			unplayable++
			continue
		}
		if seen[product.ID] {
			continue
		}
		seen[product.ID] = true
		pool = append(pool, product)
	}

	index := make(map[string]Product, len(pool))
	for _, product := range pool {
		index[product.ID] = product
	}

	productsMu.Lock()
	products = pool
	productIndex = index
	productsMu.Unlock()

	fmt.Printf("Product pool now holds %d playable products (%d unplayable dropped, %d duplicate listings skipped)\n",
		len(pool), unplayable, len(list)-unplayable-len(pool))
}

// ProductCount returns how many products are available for the daily pick.
func ProductCount() int {
	productsMu.RLock()
	defer productsMu.RUnlock()
	return len(products)
}

// ProductByID returns a playable product by id. The slump mode uses it so the
// item a game started with keeps working even after the pool was re-scraped.
func ProductByID(id string) (Product, bool) {
	productsMu.RLock()
	defer productsMu.RUnlock()

	product, ok := productIndex[id]
	return product, ok
}

// randomProduct picks a random product, avoiding avoidID when possible so that
// two consecutive days rarely show the same item.
func randomProduct(avoidID string) (Product, bool) {
	productsMu.RLock()
	defer productsMu.RUnlock()

	if len(products) == 0 {
		return Product{}, false
	}
	if len(products) == 1 || avoidID == "" {
		return products[rand.IntN(len(products))], true
	}

	for {
		product := products[rand.IntN(len(products))]
		if product.ID != avoidID {
			return product, true
		}
	}
}

func todayKey() string {
	return time.Now().Format(dailyDateLayout)
}

// EnsureDailyProduct makes sure a product is chosen for the current day. A
// product already picked today (in memory or on disk) is kept, so restarts do
// not change the answer.
func EnsureDailyProduct() error {
	dailyMu.RLock()
	current := dailyData
	dailyMu.RUnlock()

	if current != nil && current.Date == todayKey() && current.Product.ID != "" {
		return nil
	}

	if saved, err := readDailyProduct(); err == nil && saved.Date == todayKey() && saved.Product.ID != "" {
		dailyMu.Lock()
		dailyData = saved
		dailyMu.Unlock()
		fmt.Printf("Restored daily product: %s\n", saved.Product.Name)
		return nil
	}

	return PickDailyProduct()
}

// PickDailyProduct chooses a new product for today, replacing any previous
// one, and stores it on disk. It is what the midnight cron calls.
func PickDailyProduct() error {
	dailyMu.RLock()
	var avoidID string
	if dailyData != nil {
		avoidID = dailyData.Product.ID
	}
	dailyMu.RUnlock()

	product, ok := randomProduct(avoidID)
	if !ok {
		return fmt.Errorf("product pool is empty, nothing to pick from yet")
	}

	picked := &DailyProduct{Date: todayKey(), Product: product}

	dailyMu.Lock()
	dailyData = picked
	dailyMu.Unlock()

	if err := writeDailyProduct(picked); err != nil {
		fmt.Printf("Warning: could not persist daily product: %v\n", err)
	}

	fmt.Printf("Daily product for %s: %s (%.2f %s)\n",
		picked.Date, product.Name, product.Price, product.PriceUnit)

	return nil
}

// CurrentDailyProduct returns the product of the day, picking one lazily if the
// server started before the scraper had produced any products.
func CurrentDailyProduct() (Product, bool) {
	if err := EnsureDailyProduct(); err != nil {
		return Product{}, false
	}

	dailyMu.RLock()
	defer dailyMu.RUnlock()

	if dailyData == nil {
		return Product{}, false
	}
	return dailyData.Product, true
}

func readDailyProduct() (*DailyProduct, error) {
	data, err := os.ReadFile(dailyStateFile)
	if err != nil {
		return nil, err
	}

	var saved DailyProduct
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, err
	}
	return &saved, nil
}

func writeDailyProduct(daily *DailyProduct) error {
	data, err := json.Marshal(daily)
	if err != nil {
		return err
	}
	return os.WriteFile(dailyStateFile, data, 0o644)
}

// Scoring rules for a guess. The absolute distances are in kronor.
const (
	// MaxGuesses is how many guesses the player gets per day.
	MaxGuesses = 6
	// winTolerance is how close a guess has to be to count as correct
	// (50 öre), so the player does not have to hit the exact öre.
	winTolerance = 0.50
	// warmAbsolute and warmRelative decide what still counts as "warm":
	// within 5 kr or within 20% of the price.
	warmAbsolute = 5.0
	warmRelative = 0.20
)

// toleranceText spells out the win tolerance for the game instructions, so the
// wording and the rule itself cannot drift apart.
func toleranceText() string {
	return fmt.Sprintf("%.0f öre", winTolerance*100)
}

// isWarm reports whether a distance from the price still counts as warm. The
// relative rule keeps the game fair for both cheap and expensive products.
func isWarm(distance, price float64) bool {
	if distance <= warmAbsolute {
		return true
	}
	return price > 0 && distance/price <= warmRelative
}

// roundGuess rounds a guess to whole öre and reports whether it could be a
// price at all.
func roundGuess(value float64) (float64, bool) {
	guess := math.Round(value*100) / 100
	if math.IsNaN(guess) || math.IsInf(guess, 0) || guess <= 0 {
		return 0, false
	}
	return guess, true
}

// guessScore compares a guess with a price and reports which way the price is
// and how close the guess was. Both game modes score guesses through it, so
// they can never disagree about the rules.
func guessScore(product Product, guess float64) guessResponse {
	response := guessResponse{Guess: guess}

	difference := guess - product.Price
	distance := math.Abs(difference)

	switch {
	case distance <= winTolerance:
		response.Result = "correct"
		response.Warmth = "correct"
	case isWarm(distance, product.Price):
		response.Warmth = "warm"
	default:
		response.Warmth = "cold"
	}

	if response.Result == "" {
		if difference < 0 {
			response.Result = "low"
		} else {
			response.Result = "high"
		}
	}

	return response
}

// toPublicProduct strips everything the player must not see, above all the
// price, from a product.
func toPublicProduct(product Product) publicProduct {
	return publicProduct{
		ID:          product.ID,
		Name:        product.Name,
		Description: product.Description,
		ImageURL:    product.ImageURL,
		Amount:      product.Amount,
		PriceUnit:   product.PriceUnit,
		Category:    product.CategoryPath,
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		fmt.Printf("Error encoding JSON response: %v\n", err)
	}
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// DailyHandler serves the product of the day without its price.
func DailyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "endast GET stöds")
		return
	}

	product, ok := CurrentDailyProduct()
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "dagens vara är inte vald ännu")
		return
	}

	writeJSON(w, http.StatusOK, dailyResponse{
		Date:    todayKey(),
		Product: toPublicProduct(product),
	})
}

// GuessHandler compares a price guess with the product of the day and answers
// with the direction (low/high/correct) and how warm the guess was.
func GuessHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "endast POST stöds")
		return
	}

	product, ok := CurrentDailyProduct()
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "dagens vara är inte vald ännu")
		return
	}

	var request guessRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	if err := decoder.Decode(&request); err != nil {
		writeJSONError(w, http.StatusBadRequest, "ogiltig gissning")
		return
	}

	guess, ok := roundGuess(request.Guess)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "gissningen måste vara ett positivt pris")
		return
	}

	writeJSON(w, http.StatusOK, guessScore(product, guess))
}

// AnswerHandler reveals the price of the product of the day. The frontend calls
// it once the player is out of guesses (or has solved the puzzle).
func AnswerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "endast GET stöds")
		return
	}

	product, ok := CurrentDailyProduct()
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "dagens vara är inte vald ännu")
		return
	}

	writeJSON(w, http.StatusOK, answerResponse{
		Name:      product.Name,
		Amount:    product.Amount,
		Price:     product.Price,
		PriceUnit: product.PriceUnit,
	})
}
