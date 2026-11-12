package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testStridProducts() []Product {
	products := make([]Product, 0, 8)
	for index := 0; index < 8; index++ {
		products = append(products, Product{
			ID:        fmt.Sprintf("test-%d", index),
			Name:      fmt.Sprintf("Testvara %d", index),
			Price:     10.00,
			PriceUnit: "st",
			ImageURL:  "https://example.com/test.png",
		})
	}
	return products
}

// withStridTestProducts swaps the product pool for a deterministic one and
// restores it when the test finishes.
func withStridTestProducts(t *testing.T) {
	t.Helper()

	productsMu.RLock()
	oldProducts := products
	oldIndex := productIndex
	productsMu.RUnlock()

	SetProducts(testStridProducts())
	t.Cleanup(func() {
		productsMu.Lock()
		products = oldProducts
		productIndex = oldIndex
		productsMu.Unlock()
	})
}

func withShortStridRounds(t *testing.T) {
	t.Helper()

	oldGuess := stridGuessDuration
	oldReveal := stridRevealDuration
	stridGuessDuration = 200 * time.Millisecond
	stridRevealDuration = 50 * time.Millisecond
	t.Cleanup(func() {
		stridGuessDuration = oldGuess
		stridRevealDuration = oldReveal
	})
}

func stridStatusOf(err error) int {
	var httpError *stridHTTPError
	if errors.As(err, &httpError) {
		return httpError.status
	}
	return 0
}

func findStridPlayer(t *testing.T, state stridState, name string) stridPlayerState {
	t.Helper()
	for _, player := range state.Players {
		if player.Name == name {
			return player
		}
	}
	t.Fatalf("player %q not found in state", name)
	return stridPlayerState{}
}

func waitForStridState(t *testing.T, lobby *stridLobby, description string, condition func(stridState) bool) stridState {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		lobby.mu.Lock()
		viewer := lobby.players[0]
		state := lobby.snapshotLocked(viewer)
		lobby.mu.Unlock()

		if condition(state) {
			return state
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", description)
	return stridState{}
}

func TestStridScore(t *testing.T) {
	tests := []struct {
		name     string
		guessOre int
		priceOre int
		want     int
	}{
		{name: "exact", guessOre: 2490, priceOre: 2490, want: 5000},
		{name: "one ore off", guessOre: 2491, priceOre: 2490, want: 4998},
		{name: "half price off", guessOre: 500, priceOre: 1000, want: 2500},
		{name: "a full price off", guessOre: 2000, priceOre: 1000, want: 0},
		{name: "zero price", guessOre: 100, priceOre: 0, want: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := stridScore(test.guessOre, test.priceOre); got != test.want {
				t.Fatalf("stridScore(%d, %d) = %d, want %d", test.guessOre, test.priceOre, got, test.want)
			}
		})
	}

	// The score must never increase as the guess moves farther away.
	previous := stridMaxPoints + 1
	for distance := 0; distance <= 2490; distance++ {
		score := stridScore(2490+distance, 2490)
		if score > previous {
			t.Fatalf("score grew with distance: %d > %d", score, previous)
		}
		previous = score
	}
}

func TestNormalizeStridCode(t *testing.T) {
	valid := map[string]string{
		"abcd":   "ABCD",
		" A1b2 ": "A1B2",
		"0000":   "0000",
	}
	for input, want := range valid {
		got, ok := normalizeStridCode(input)
		if !ok || got != want {
			t.Fatalf("normalizeStridCode(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}

	invalid := []string{"", "ABC", "ABCDE", "AB-D", "ÅÄÖ1"}
	for _, input := range invalid {
		if got, ok := normalizeStridCode(input); ok {
			t.Fatalf("normalizeStridCode(%q) = %q, true; want invalid", input, got)
		}
	}
}

func TestStridLobbyLifecycle(t *testing.T) {
	withStridTestProducts(t)
	withShortStridRounds(t)

	store := newStridStore()
	lobby, host, err := store.create()
	if err != nil {
		t.Fatalf("create lobby: %v", err)
	}
	if len(lobby.code) != 4 {
		t.Fatalf("code %q is not 4 characters", lobby.code)
	}

	// One player is not enough.
	if err := lobby.start(host.Token); stridStatusOf(err) != http.StatusConflict {
		t.Fatalf("start with one player: got %v", err)
	}

	if err := lobby.ready(host.Token, "Anna"); err != nil {
		t.Fatalf("host ready: %v", err)
	}
	// Ready is final.
	if err := lobby.ready(host.Token, "Annan"); stridStatusOf(err) != http.StatusConflict {
		t.Fatalf("second ready: got %v", err)
	}

	bo, err := lobby.join("Bo")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	// Names are unique, case-insensitively.
	if _, err := lobby.join("anna"); stridStatusOf(err) != http.StatusConflict {
		t.Fatalf("duplicate name: got %v", err)
	}

	state, err := lobby.state(host.Token)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if !state.CanStart {
		t.Fatalf("host should be allowed to start: %+v", state)
	}
	if state.Players[0].Name != "Anna" || !state.Players[0].Ready {
		t.Fatalf("host view is wrong: %+v", state.Players[0])
	}

	if err := lobby.start(host.Token); err != nil {
		t.Fatalf("start: %v", err)
	}

	state, err = lobby.state(host.Token)
	if err != nil {
		t.Fatalf("state after start: %v", err)
	}
	if state.Status != stridStatusPlaying || state.Phase != stridPhaseGuessing || state.Round == nil {
		t.Fatalf("unexpected match state: %+v", state)
	}
	if state.Round.Number != 1 || state.Round.Total != stridRounds {
		t.Fatalf("unexpected round: %+v", state.Round)
	}
	if state.Round.Price != nil {
		t.Fatalf("price leaked during guessing: %v", *state.Round.Price)
	}

	// The product has price 10.00 in the test pool, so an exact guess is known.
	if _, err := lobby.guess(host.Token, 10); err != nil {
		t.Fatalf("host guess: %v", err)
	}

	// Until everybody has guessed, the round stays open and only the player's
	// own guess is visible.
	state, err = lobby.state(host.Token)
	if err != nil {
		t.Fatalf("state during guessing: %v", err)
	}
	if state.Phase != stridPhaseGuessing {
		t.Fatalf("round closed before everybody guessed: %+v", state)
	}
	hostView := findStridPlayer(t, state, "Anna")
	boView := findStridPlayer(t, state, "Bo")
	if hostView.Guess == nil || *hostView.Guess != 10 {
		t.Fatalf("own guess missing: %+v", hostView)
	}
	if boView.Guessed || boView.Guess != nil {
		t.Fatalf("opponent guess leaked: %+v", boView)
	}

	if _, err := lobby.guess(host.Token, 11); stridStatusOf(err) != http.StatusConflict {
		t.Fatalf("second guess: got %v", err)
	}
	if _, err := lobby.guess(bo.Token, 20); err != nil {
		t.Fatalf("bo guess: %v", err)
	}

	// The last guess closes the round immediately instead of waiting for the
	// ten second timer.
	state, err = lobby.state(host.Token)
	if err != nil {
		t.Fatalf("state after the last guess: %v", err)
	}
	if state.Phase != stridPhaseReveal {
		t.Fatalf("round did not close when everybody had guessed: %+v", state)
	}
	if state.Round == nil || state.Round.Price == nil {
		t.Fatalf("price not revealed: %+v", state)
	}
	hostView = findStridPlayer(t, state, "Anna")
	boView = findStridPlayer(t, state, "Bo")
	if hostView.Guess == nil || *hostView.Guess != 10 || !hostView.Closest || !hostView.Exact {
		t.Fatalf("host reveal is wrong: %+v", hostView)
	}
	if hostView.Points == nil || *hostView.Points != 5000 {
		t.Fatalf("host points = %v, want 5000", hostView.Points)
	}
	if boView.Points == nil || *boView.Points != 0 {
		t.Fatalf("bo points = %v, want 0", boView.Points)
	}
	if state.Players[0].Score != 5000 {
		t.Fatalf("host total score = %d, want 5000", state.Players[0].Score)
	}

	final := waitForStridState(t, lobby, "the finished match", func(state stridState) bool {
		return state.Status == stridStatusFinished
	})
	if final.Phase != stridPhaseFinished || final.Round == nil || final.Round.Number != stridRounds {
		t.Fatalf("unexpected final state: %+v", final)
	}
	if len(final.Winners) != 1 || final.Winners[0] != "Anna" {
		t.Fatalf("winners = %v, want [Anna]", final.Winners)
	}
	if final.Players[0].Place != 1 || !final.Players[0].Winner {
		t.Fatalf("host placement is wrong: %+v", final.Players[0])
	}

	if final.Recap == nil {
		t.Fatal("final state has no recap")
	}
	if len(final.Recap.Players) != 2 || final.Recap.Players[0] != "Anna" || final.Recap.Players[1] != "Bo" {
		t.Fatalf("recap players = %v", final.Recap.Players)
	}
	if len(final.Recap.Rounds) != stridRounds {
		t.Fatalf("recap rounds = %d, want %d", len(final.Recap.Rounds), stridRounds)
	}

	firstRound := final.Recap.Rounds[0]
	if firstRound.Number != 1 || len(firstRound.Guesses) != 2 || len(firstRound.Points) != 2 {
		t.Fatalf("unexpected first recap round: %+v", firstRound)
	}
	if firstRound.Guesses[0] == nil || *firstRound.Guesses[0] != 10 {
		t.Fatalf("Anna's recap guess = %v, want 10", firstRound.Guesses[0])
	}
	if firstRound.Guesses[1] == nil || *firstRound.Guesses[1] != 20 {
		t.Fatalf("Bo's recap guess = %v, want 20", firstRound.Guesses[1])
	}
	if firstRound.Points[0] != 5000 || firstRound.Points[1] != 0 {
		t.Fatalf("first recap points = %v, want [5000 0]", firstRound.Points)
	}

	for _, round := range final.Recap.Rounds[1:] {
		for index, guess := range round.Guesses {
			if guess != nil {
				t.Fatalf("round %d guess %d = %v, want no guess", round.Number, index, *guess)
			}
		}
	}

	if _, err := lobby.guess(host.Token, 10); stridStatusOf(err) != http.StatusConflict {
		t.Fatalf("guess after finish: got %v", err)
	}

	// "Spela igen" takes the finished lobby back to the waiting room with the
	// same players, names and ready flags, but reset scores.
	if err := lobby.playAgain(host.Token); err != nil {
		t.Fatalf("play again: %v", err)
	}

	state, err = lobby.state(host.Token)
	if err != nil {
		t.Fatalf("state after play again: %v", err)
	}
	if state.Status != stridStatusWaiting || state.Phase != stridPhaseLobby {
		t.Fatalf("unexpected state after play again: %+v", state)
	}
	if state.Round != nil || state.Recap != nil || len(state.Winners) != 0 {
		t.Fatalf("old match data survived play again: %+v", state)
	}
	if len(state.Players) != 2 || !state.CanStart {
		t.Fatalf("lobby should be startable again: %+v", state)
	}
	for _, player := range state.Players {
		if player.Score != 0 || !player.Ready || player.Name == "" {
			t.Fatalf("player was not reset correctly: %+v", player)
		}
	}

	if err := lobby.playAgain(host.Token); stridStatusOf(err) != http.StatusConflict {
		t.Fatalf("second play again: got %v", err)
	}
}

func TestStridLobbyIsCappedAtFivePlayers(t *testing.T) {
	store := newStridStore()
	lobby, host, err := store.create()
	if err != nil {
		t.Fatalf("create lobby: %v", err)
	}
	if err := lobby.ready(host.Token, "Spelare 1"); err != nil {
		t.Fatalf("host ready: %v", err)
	}

	for index := 2; index <= stridMaxPlayers; index++ {
		if _, err := lobby.join(fmt.Sprintf("Spelare %d", index)); err != nil {
			t.Fatalf("join player %d: %v", index, err)
		}
	}

	if _, err := lobby.join("För många"); stridStatusOf(err) != http.StatusConflict {
		t.Fatalf("sixth player: got %v", err)
	}
}

func TestStridAPI(t *testing.T) {
	withStridTestProducts(t)
	withShortStridRounds(t)

	store := openTestGuessStore(t)
	useGuessStoreForTest(t, store)

	server := httptest.NewServer(newMux())
	defer server.Close()

	create := stridPostJSON(t, server.URL+"/api/strid/lobbies", map[string]any{})
	if create.Code == "" || create.Token == "" {
		t.Fatalf("unexpected create response: %+v", create)
	}

	join := stridPostJSON(t, server.URL+"/api/strid/join", map[string]string{
		"code": create.Code,
		"name": "Bo",
	})
	if join.Token == "" || join.Code != create.Code {
		t.Fatalf("unexpected join response: %+v", join)
	}

	stridPostStatus(t, server.URL+"/api/strid/ready", map[string]string{
		"code":  create.Code,
		"token": create.Token,
		"name":  "Anna",
	}, http.StatusOK)

	stridPostStatus(t, server.URL+"/api/strid/start", map[string]string{
		"code":  create.Code,
		"token": create.Token,
	}, http.StatusOK)

	stateURL := server.URL + "/api/strid/state?code=" + create.Code + "&token=" + create.Token
	response, err := http.Get(stateURL)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("state status = %d", response.StatusCode)
	}

	var state stridState
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.Status != stridStatusPlaying || state.Round == nil {
		t.Fatalf("unexpected state: %+v", state)
	}

	stridPostStatus(t, server.URL+"/api/strid/guess", map[string]any{
		"code":  create.Code,
		"token": create.Token,
		"guess": 10,
	}, http.StatusOK)

	guesses := refreshAndLoadGuessStats(t, store)
	if got, want := guesses.TotalGuesses, 1; got != want {
		t.Fatalf("strid guess was not saved: TotalGuesses = %d, want %d", got, want)
	}
	if got, want := guesses.MedianErrorText, "0"+nbsp+"%"; got != want {
		t.Errorf("strid guess median = %q, want %q", got, want)
	}

	// The event stream must start with a state event for the same player.
	request, err := http.NewRequest(http.MethodGet,
		server.URL+"/api/strid/events?code="+create.Code+"&token="+create.Token, nil)
	if err != nil {
		t.Fatalf("create event request: %v", err)
	}
	stream, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer stream.Body.Close()

	scanner := bufio.NewScanner(stream.Body)
	found := false
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "event: state") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no state event on the stream: %v", scanner.Err())
	}

	// Let the match run through all five short rounds before the test restores
	// the normal round durations, so no timer outlives the test.
	lobby := stridLobbies.lookup(create.Code)
	if lobby == nil {
		t.Fatalf("API lobby %q vanished", create.Code)
	}
	waitForStridState(t, lobby, "the API match to finish", func(state stridState) bool {
		return state.Status == stridStatusFinished
	})

	stridPostStatus(t, server.URL+"/api/strid/again", map[string]string{
		"code":  create.Code,
		"token": create.Token,
	}, http.StatusOK)

	resetState := waitForStridState(t, lobby, "the lobby to reset", func(state stridState) bool {
		return state.Status == stridStatusWaiting
	})
	if resetState.Round != nil || !resetState.CanStart {
		t.Fatalf("unexpected reset state: %+v", resetState)
	}
	for _, player := range resetState.Players {
		if player.Score != 0 {
			t.Fatalf("score was not reset: %+v", player)
		}
	}
}

type stridTestCreateResponse struct {
	Code  string `json:"code"`
	Token string `json:"token"`
}

func stridPostJSON(t *testing.T, url string, body any) stridTestCreateResponse {
	t.Helper()

	status, payload := stridDoPost(t, url, body)
	if status != http.StatusOK {
		t.Fatalf("POST %s = %d: %s", url, status, payload)
	}

	var response stridTestCreateResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("decode response from %s: %v", url, err)
	}
	return response
}

func stridPostStatus(t *testing.T, url string, body any, want int) {
	t.Helper()

	status, payload := stridDoPost(t, url, body)
	if status != want {
		t.Fatalf("POST %s = %d, want %d: %s", url, status, want, payload)
	}
}

func stridDoPost(t *testing.T, url string, body any) (int, []byte) {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	response, err := http.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer response.Body.Close()

	var buffer bytes.Buffer
	if _, err := buffer.ReadFrom(response.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response.StatusCode, buffer.Bytes()
}
