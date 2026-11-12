package server

import (
	"encoding/json"
	"net/http"
)

// The slump mode ("Jag har tur") lets the player guess the price of a random
// product. The product travels in the URL (?id=...) instead of in server side
// state: a reload keeps playing the same item, and a shared link gives a friend
// the same item. The price never leaves the server until the game is over.

type slumpProductResponse struct {
	ID      string        `json:"id"`
	Product publicProduct `json:"product"`
}

type slumpGuessRequest struct {
	ID    string  `json:"id"`
	Guess float64 `json:"guess"`
}

// slumpProduct returns the product to play: the requested one as long as it is
// still in the pool, a random one otherwise.
func slumpProduct(id string) (Product, bool) {
	if product, ok := ProductByID(id); ok {
		return product, true
	}
	return randomProduct("")
}

// SlumpPage renders a random product to guess the price of. The page script
// repeats the id in the URL, so a reload does not swap the product.
func SlumpPage(w http.ResponseWriter, r *http.Request) {
	product, ok := slumpProduct(r.URL.Query().Get("id"))

	data := gamePageData{
		pageData: pageData{
			Title:      "IKEAdle - Jag har tur",
			ActiveLink: "/slump",
			Modes:      modes,
		},
		Product:    productCardFrom(product, ok),
		MaxGuesses: MaxGuesses,
		Tolerance:  toleranceText(),
	}
	if ok {
		data.ProductID = product.ID
	}

	renderPage(w, slumpPage, data)
}

// SlumpProductHandler serves a random product without its price. The page uses
// it when it was rendered before the scraper had produced any products.
func SlumpProductHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "endast GET stöds")
		return
	}

	product, ok := slumpProduct("")
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "det finns inga varor att spela med ännu")
		return
	}

	writeJSON(w, http.StatusOK, slumpProductResponse{
		ID:      product.ID,
		Product: toPublicProduct(product),
	})
}

// SlumpGuessHandler scores a guess against the product the client is playing.
func SlumpGuessHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "endast POST stöds")
		return
	}

	var request slumpGuessRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	if err := decoder.Decode(&request); err != nil {
		writeJSONError(w, http.StatusBadRequest, "ogiltig gissning")
		return
	}

	product, ok := ProductByID(request.ID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "varan finns inte längre, dra en ny")
		return
	}

	guess, ok := roundGuess(request.Guess)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "gissningen måste vara ett positivt pris")
		return
	}

	response := guessScore(product, guess)
	recordGuess(guessModeSlump, product, guess)
	writeJSON(w, http.StatusOK, response)
}

// SlumpAnswerHandler reveals the price of the product a slump game is playing.
func SlumpAnswerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "endast GET stöds")
		return
	}

	product, ok := ProductByID(r.URL.Query().Get("id"))
	if !ok {
		writeJSONError(w, http.StatusNotFound, "varan finns inte längre")
		return
	}

	writeJSON(w, http.StatusOK, answerResponse{
		Name:      product.Name,
		Amount:    product.Amount,
		Price:     product.Price,
		PriceUnit: product.PriceUnit,
	})
}
