package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type stridCreateResponse struct {
	Code  string `json:"code"`
	Token string `json:"token"`
}

type stridJoinRequest struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type stridReadyRequest struct {
	Code  string `json:"code"`
	Token string `json:"token"`
	Name  string `json:"name"`
}

type stridTokenRequest struct {
	Code  string `json:"code"`
	Token string `json:"token"`
}

type stridGuessRequest struct {
	Code  string  `json:"code"`
	Token string  `json:"token"`
	Guess float64 `json:"guess"`
}

type stridGuessResponse struct {
	Guess float64 `json:"guess"`
}

type stridOKResponse struct {
	OK bool `json:"ok"`
}

func writeStridError(w http.ResponseWriter, err error) {
	var httpError *stridHTTPError
	if errors.As(err, &httpError) {
		writeJSONError(w, httpError.status, httpError.message)
		return
	}

	fmt.Printf("Strid error: %v\n", err)
	writeJSONError(w, http.StatusInternalServerError, "något gick fel")
}

func decodeStridJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := decoder.Decode(destination); err != nil {
		writeJSONError(w, http.StatusBadRequest, "ogiltig förfrågan")
		return false
	}
	return true
}

// StridCreateHandler creates a lobby and returns the code others can join
// with, plus the host's token.
func StridCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "endast POST stöds")
		return
	}

	lobby, host, err := stridLobbies.create()
	if err != nil {
		writeStridError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, stridCreateResponse{
		Code:  lobby.code,
		Token: host.Token,
	})
}

// StridJoinHandler joins a lobby and marks the player ready in one step, since
// the name is already chosen on the join screen.
func StridJoinHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "endast POST stöds")
		return
	}

	var request stridJoinRequest
	if !decodeStridJSON(w, r, &request) {
		return
	}

	lobby, err := stridLobbies.lookupCode(request.Code)
	if err != nil {
		writeStridError(w, err)
		return
	}

	player, err := lobby.join(request.Name)
	if err != nil {
		writeStridError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, stridCreateResponse{
		Code:  lobby.code,
		Token: player.Token,
	})
}

// StridReadyHandler lets the host submit the username it picked in the lobby.
func StridReadyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "endast POST stöds")
		return
	}

	var request stridReadyRequest
	if !decodeStridJSON(w, r, &request) {
		return
	}

	lobby, err := stridLobbies.lookupCode(request.Code)
	if err != nil {
		writeStridError(w, err)
		return
	}
	if err := lobby.ready(request.Token, request.Name); err != nil {
		writeStridError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, stridOKResponse{OK: true})
}

// StridStartHandler lets the host start the match once everyone is ready.
func StridStartHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "endast POST stöds")
		return
	}

	var request stridTokenRequest
	if !decodeStridJSON(w, r, &request) {
		return
	}

	lobby, err := stridLobbies.lookupCode(request.Code)
	if err != nil {
		writeStridError(w, err)
		return
	}
	if err := lobby.start(request.Token); err != nil {
		writeStridError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, stridOKResponse{OK: true})
}

// StridAgainHandler returns a finished lobby to its waiting room so the same
// group can play another match.
func StridAgainHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "endast POST stöds")
		return
	}

	var request stridTokenRequest
	if !decodeStridJSON(w, r, &request) {
		return
	}

	lobby, err := stridLobbies.lookupCode(request.Code)
	if err != nil {
		writeStridError(w, err)
		return
	}
	if err := lobby.playAgain(request.Token); err != nil {
		writeStridError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, stridOKResponse{OK: true})
}

// StridGuessHandler locks in one price guess for the current round.
func StridGuessHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "endast POST stöds")
		return
	}

	var request stridGuessRequest
	if !decodeStridJSON(w, r, &request) {
		return
	}

	lobby, err := stridLobbies.lookupCode(request.Code)
	if err != nil {
		writeStridError(w, err)
		return
	}

	guess, err := lobby.guess(request.Token, request.Guess)
	if err != nil {
		writeStridError(w, err)
		return
	}

	recordGuess(guessModeStrid, guess.Product, guess.Guess)
	writeJSON(w, http.StatusOK, stridGuessResponse{Guess: guess.Guess})
}

// StridStateHandler returns the full state for one player. The client uses it
// for the first paint and as a fallback if the event stream is unavailable.
func StridStateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "endast GET stöds")
		return
	}

	lobby, err := stridLobbies.lookupCode(r.URL.Query().Get("code"))
	if err != nil {
		writeStridError(w, err)
		return
	}

	state, err := lobby.state(r.URL.Query().Get("token"))
	if err != nil {
		writeStridError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, state)
}

// StridEventsHandler streams the player's view of the lobby as server-sent
// events. Every change on the server is pushed immediately; a small heartbeat
// keeps idle connections alive through proxies.
func StridEventsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "endast GET stöds")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "strömning stöds inte")
		return
	}

	lobby, err := stridLobbies.lookupCode(r.URL.Query().Get("code"))
	if err != nil {
		writeStridError(w, err)
		return
	}

	subscriber, err := lobby.subscribe(r.URL.Query().Get("token"))
	if err != nil {
		writeStridError(w, err)
		return
	}
	defer lobby.unsubscribe(subscriber)

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case state := <-subscriber.ch:
			payload, err := json.Marshal(state)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
