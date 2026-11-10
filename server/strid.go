package server

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// The "Kund mot kund" mode is a small realtime game. A lobby holds 2-5
// players, a host starts a match of five rounds and every round gives all
// players the same product, one guess each and ten seconds to make it.
//
// Everything lives in memory: a lobby is created by a player, and its state is
// pushed to every member over server-sent events. When the last player has
// left, the janitor eventually removes the lobby.

const (
	stridMinPlayers = 2
	stridMaxPlayers = 5

	stridRounds        = 5
	stridGuessSeconds  = 10
	stridRevealSeconds = 6

	// stridMaxPoints is the score for guessing the exact price down to the
	// öre. Every other guess scores less; see stridScore.
	stridMaxPoints = 5000

	stridCodeLength = 4
	stridNameRunes  = 20
)

// The durations are variables so tests can shorten a match.
var (
	stridGuessDuration  = time.Duration(stridGuessSeconds) * time.Second
	stridRevealDuration = time.Duration(stridRevealSeconds) * time.Second
)

const (
	stridAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	stridStatusWaiting  = "waiting"
	stridStatusPlaying  = "playing"
	stridStatusFinished = "finished"

	stridPhaseLobby    = "lobby"
	stridPhaseGuessing = "guessing"
	stridPhaseReveal   = "reveal"
	stridPhaseFinished = "finished"

	// The states a lobby code can be in when only the code is known, i.e. for
	// someone who opens an invite link without being a member yet.
	stridCodeOpen     = "open"
	stridCodeMissing  = "missing"
	stridCodeStarted  = "started"
	stridCodeFinished = "finished"
	stridCodeFull     = "full"

	stridLobbyTTL    = 2 * time.Hour
	stridFinishedTTL = 6 * time.Hour
)

// stridHTTPError is an error that already knows which status and Swedish
// message the API should answer with.
type stridHTTPError struct {
	status  int
	message string
}

func (e *stridHTTPError) Error() string { return e.message }

func newStridError(status int, message string) error {
	return &stridHTTPError{status: status, message: message}
}

type stridPlayer struct {
	// Token is the player's identity. It never leaves the server except to the
	// player itself, and is what reconnects a refreshed page to the same seat.
	Token string
	Name  string
	Host  bool
	Ready bool

	Score      int
	ExactCount int

	// connections counts the open event streams for this player, so the lobby
	// can show whether someone is online.
	connections int
}

type stridGuess struct {
	Value float64
	Ore   int
}

type stridMatch struct {
	Round    int
	Phase    string
	Products []Product

	// Guesses are keyed by player token. A guess is locked as soon as it is
	// stored and is never replaced.
	Guesses map[string]stridGuess

	// History keeps every closed round so the final screen can show a recap
	// of all products and all guesses.
	History []stridRoundRecord

	// The fields below are filled in when a round closes and are what the
	// reveal phase shows.
	RoundPoints    map[string]int
	RoundDistances map[string]int // in öre
	ClosestOre     int            // -1 when nobody guessed
	HasResults     bool

	RoundEndsAt  time.Time
	RevealEndsAt time.Time
}

// stridRoundRecord is one finished round as it looked when it closed.
type stridRoundRecord struct {
	Round   int
	Product Product
	Guesses map[string]stridGuess
	Points  map[string]int
}

type stridSubscriber struct {
	playerToken string
	ch          chan stridState
}

// deliver puts a state on the subscriber's channel. If the client is slow, the
// oldest state is dropped so the newest one is never lost.
func (s *stridSubscriber) deliver(state stridState) {
	select {
	case s.ch <- state:
		return
	default:
	}

	select {
	case <-s.ch:
	default:
	}

	select {
	case s.ch <- state:
	default:
	}
}

type stridLobby struct {
	mu sync.Mutex

	code    string
	players []*stridPlayer
	status  string
	match   *stridMatch

	createdAt    time.Time
	lastActivity time.Time

	subscribers map[*stridSubscriber]string // subscriber -> player token
	// revision increases with every broadcast. Clients use it to ignore a
	// state response that was created before a newer event stream update.
	revision uint64
	// timerGeneration invalidates timers that belong to an older round, which
	// keeps a stale callback from advancing a match twice.
	timerGeneration int
}

type stridStore struct {
	mu      sync.Mutex
	lobbies map[string]*stridLobby

	janitorOnce sync.Once
}

var stridLobbies = newStridStore()

func newStridStore() *stridStore {
	return &stridStore{lobbies: make(map[string]*stridLobby)}
}

func (s *stridStore) startJanitor() {
	s.janitorOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				s.cleanup(time.Now())
			}
		}()
	})
}

func (s *stridStore) cleanup(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for code, lobby := range s.lobbies {
		lobby.mu.Lock()
		idle := len(lobby.subscribers) == 0 && now.Sub(lobby.lastActivity) > stridLobbyTTL
		if lobby.status == stridStatusFinished && now.Sub(lobby.lastActivity) > stridFinishedTTL {
			idle = true
		}
		if idle {
			// Stop any pending round timer along with the lobby.
			lobby.timerGeneration++
			delete(s.lobbies, code)
		}
		lobby.mu.Unlock()
	}
}

// create makes a lobby and its host. The host has not picked a name or marked
// itself ready yet; that is what the first Redo button in the lobby does.
func (s *stridStore) create() (*stridLobby, *stridPlayer, error) {
	token, err := newStridToken()
	if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	host := &stridPlayer{Token: token, Host: true}
	lobby := &stridLobby{
		players:      []*stridPlayer{host},
		status:       stridStatusWaiting,
		createdAt:    now,
		lastActivity: now,
		subscribers:  make(map[*stridSubscriber]string),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for attempt := 0; attempt < 100; attempt++ {
		code, err := newStridCode()
		if err != nil {
			return nil, nil, err
		}
		if _, exists := s.lobbies[code]; exists {
			continue
		}
		lobby.code = code
		s.lobbies[code] = lobby
		return lobby, host, nil
	}

	return nil, nil, fmt.Errorf("could not allocate a free lobby code")
}

func (s *stridStore) lookup(code string) *stridLobby {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lobbies[code]
}

// lookupCode normalizes a code and returns its lobby, with an error suitable
// for the API when the code is invalid or unknown.
func (s *stridStore) lookupCode(raw string) (*stridLobby, error) {
	code, ok := normalizeStridCode(raw)
	if !ok {
		return nil, newStridError(http.StatusBadRequest, "ogiltig lobbykod")
	}

	lobby := s.lookup(code)
	if lobby == nil {
		return nil, newStridError(http.StatusNotFound, "lobbyn finns inte")
	}
	return lobby, nil
}

// codeState describes a lobby to someone who only knows the code. It is used
// to render the invite page before a player has joined.
func (s *stridStore) codeState(code string) string {
	lobby := s.lookup(code)
	if lobby == nil {
		return stridCodeMissing
	}

	lobby.mu.Lock()
	defer lobby.mu.Unlock()

	switch {
	case lobby.status == stridStatusFinished:
		return stridCodeFinished
	case lobby.status == stridStatusPlaying:
		return stridCodeStarted
	case len(lobby.players) >= stridMaxPlayers:
		return stridCodeFull
	default:
		return stridCodeOpen
	}
}

func (l *stridLobby) touchLocked() {
	l.lastActivity = time.Now()
}

func (l *stridLobby) playerLocked(token string) *stridPlayer {
	if token == "" {
		return nil
	}
	for _, player := range l.players {
		if player.Token == token {
			return player
		}
	}
	return nil
}

func (l *stridLobby) memberLocked(token string) (*stridPlayer, error) {
	player := l.playerLocked(token)
	if player == nil {
		return nil, newStridError(http.StatusUnauthorized, "du är inte med i den här lobbyn")
	}
	return player, nil
}

func (l *stridLobby) nameTakenLocked(name string, except *stridPlayer) bool {
	for _, player := range l.players {
		if player == except || player.Name == "" {
			continue
		}
		if strings.EqualFold(player.Name, name) {
			return true
		}
	}
	return false
}

// join adds a player who has already chosen a name and pressed Redo. Joining
// and becoming ready are deliberately one step: a player who never readies
// would otherwise block the host from starting.
func (l *stridLobby) join(rawName string) (*stridPlayer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.status != stridStatusWaiting {
		return nil, newStridError(http.StatusConflict, "matchen har redan startat")
	}
	if len(l.players) >= stridMaxPlayers {
		return nil, newStridError(http.StatusConflict, "lobbyn är full (max 5 spelare)")
	}

	name, ok := normalizeStridName(rawName)
	if !ok {
		return nil, newStridError(http.StatusBadRequest, "ange ett användarnamn (1-20 tecken)")
	}
	if l.nameTakenLocked(name, nil) {
		return nil, newStridError(http.StatusConflict, "namnet är redan taget")
	}

	token, err := newStridToken()
	if err != nil {
		return nil, err
	}

	player := &stridPlayer{Token: token, Name: name, Ready: true}
	l.players = append(l.players, player)
	l.touchLocked()
	l.broadcastLocked()
	return player, nil
}

// ready locks in the host's name. A ready player can never become unready or
// rename themselves.
func (l *stridLobby) ready(token, rawName string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	player, err := l.memberLocked(token)
	if err != nil {
		return err
	}
	if l.status != stridStatusWaiting {
		return newStridError(http.StatusConflict, "matchen har redan startat")
	}
	if player.Ready {
		return newStridError(http.StatusConflict, "du är redan redo")
	}

	name, ok := normalizeStridName(rawName)
	if !ok {
		return newStridError(http.StatusBadRequest, "ange ett användarnamn (1-20 tecken)")
	}
	if l.nameTakenLocked(name, player) {
		return newStridError(http.StatusConflict, "namnet är redan taget")
	}

	player.Name = name
	player.Ready = true
	l.touchLocked()
	l.broadcastLocked()
	return nil
}

func (l *stridLobby) start(token string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	host, err := l.memberLocked(token)
	if err != nil {
		return err
	}
	if l.status != stridStatusWaiting {
		return newStridError(http.StatusConflict, "matchen har redan startat")
	}
	if !host.Host {
		return newStridError(http.StatusForbidden, "bara den som skapade lobbyn kan starta matchen")
	}
	if len(l.players) < stridMinPlayers {
		return newStridError(http.StatusConflict, "minst 2 spelare behövs")
	}
	if len(l.players) > stridMaxPlayers {
		return newStridError(http.StatusConflict, "lobbyn är full")
	}
	for _, player := range l.players {
		if !player.Ready {
			return newStridError(http.StatusConflict, "alla spelare måste vara redo")
		}
	}

	products, err := pickStridProducts(stridRounds)
	if err != nil {
		return newStridError(http.StatusServiceUnavailable, "det finns inga varor att spela med ännu")
	}

	l.match = &stridMatch{
		Round:    1,
		Products: products,
	}
	l.status = stridStatusPlaying
	l.beginRoundLocked(time.Now())
	l.touchLocked()
	l.broadcastLocked()
	return nil
}

// playAgain returns a finished lobby to the waiting room with the same
// players, names and ready flags. Scores are cleared, so the host can start a
// completely new match without anyone having to create or join a new lobby.
func (l *stridLobby) playAgain(token string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, err := l.memberLocked(token); err != nil {
		return err
	}
	if l.status != stridStatusFinished {
		return newStridError(http.StatusConflict, "matchen är inte slut ännu")
	}

	l.status = stridStatusWaiting
	l.match = nil
	// Invalidate any timer that might still be associated with the old match.
	l.timerGeneration++

	for _, player := range l.players {
		player.Score = 0
		player.ExactCount = 0
	}

	l.touchLocked()
	l.broadcastLocked()
	return nil
}

func (l *stridLobby) guess(token string, raw float64) (float64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	player, err := l.memberLocked(token)
	if err != nil {
		return 0, err
	}
	if l.status != stridStatusPlaying || l.match == nil || l.match.Phase != stridPhaseGuessing {
		return 0, newStridError(http.StatusConflict, "rundan är inte öppen för gissningar")
	}
	if time.Now().After(l.match.RoundEndsAt) {
		return 0, newStridError(http.StatusConflict, "tiden är ute för den här rundan")
	}
	if _, exists := l.match.Guesses[player.Token]; exists {
		return 0, newStridError(http.StatusConflict, "du har redan gissat")
	}

	guess, ok := roundGuess(raw)
	if !ok {
		return 0, newStridError(http.StatusBadRequest, "gissningen måste vara ett positivt pris")
	}

	l.match.Guesses[player.Token] = stridGuess{
		Value: guess,
		Ore:   priceToOre(guess),
	}

	// Nobody has to wait out the clock once every player has guessed. Closing
	// the round here also replaces the pending round timer, so it cannot fire
	// a second time.
	if len(l.match.Guesses) >= len(l.players) {
		l.closeRoundLocked()
	}

	l.touchLocked()
	l.broadcastLocked()
	return guess, nil
}

func (l *stridLobby) state(token string) (stridState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	player, err := l.memberLocked(token)
	if err != nil {
		return stridState{}, err
	}
	l.touchLocked()
	return l.snapshotLocked(player), nil
}

func (l *stridLobby) subscribe(token string) (*stridSubscriber, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	player, err := l.memberLocked(token)
	if err != nil {
		return nil, err
	}

	player.connections++
	subscriber := &stridSubscriber{
		playerToken: player.Token,
		ch:          make(chan stridState, 16),
	}
	l.subscribers[subscriber] = player.Token
	l.touchLocked()
	// The broadcast delivers the current state to the new subscriber as well
	// as the updated online status to everyone else.
	l.broadcastLocked()
	return subscriber, nil
}

func (l *stridLobby) unsubscribe(subscriber *stridSubscriber) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, ok := l.subscribers[subscriber]; !ok {
		return
	}
	delete(l.subscribers, subscriber)

	if player := l.playerLocked(subscriber.playerToken); player != nil && player.connections > 0 {
		player.connections--
	}
	l.broadcastLocked()
}

func (l *stridLobby) broadcastLocked() {
	l.revision++
	for subscriber, token := range l.subscribers {
		player := l.playerLocked(token)
		if player == nil {
			continue
		}
		subscriber.deliver(l.snapshotLocked(player))
	}
}

func (l *stridLobby) scheduleLocked(duration time.Duration) {
	l.timerGeneration++
	generation := l.timerGeneration
	time.AfterFunc(duration, func() {
		l.onTimer(generation)
	})
}

func (l *stridLobby) onTimer(generation int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if generation != l.timerGeneration || l.status != stridStatusPlaying || l.match == nil {
		return
	}

	switch l.match.Phase {
	case stridPhaseGuessing:
		l.closeRoundLocked()
	case stridPhaseReveal:
		l.nextRoundLocked()
	default:
		return
	}

	l.touchLocked()
	l.broadcastLocked()
}

func (l *stridLobby) beginRoundLocked(now time.Time) {
	match := l.match
	match.Phase = stridPhaseGuessing
	match.Guesses = make(map[string]stridGuess, len(l.players))
	match.RoundPoints = nil
	match.RoundDistances = nil
	match.ClosestOre = -1
	match.HasResults = false
	match.RevealEndsAt = time.Time{}
	match.RoundEndsAt = now.Add(stridGuessDuration)
	l.scheduleLocked(stridGuessDuration)
}

func (l *stridLobby) closeRoundLocked() {
	match := l.match
	if match == nil || match.HasResults || match.Round < 1 || match.Round > len(match.Products) {
		return
	}

	product := match.Products[match.Round-1]
	priceOre := priceToOre(product.Price)
	match.Phase = stridPhaseReveal
	match.HasResults = true
	match.RevealEndsAt = time.Now().Add(stridRevealDuration)
	match.RoundPoints = make(map[string]int, len(l.players))
	match.RoundDistances = make(map[string]int, len(l.players))

	closest := -1
	for _, player := range l.players {
		guess, ok := match.Guesses[player.Token]
		if !ok {
			match.RoundPoints[player.Token] = 0
			continue
		}

		distance := guess.Ore - priceOre
		if distance < 0 {
			distance = -distance
		}

		points := stridScore(guess.Ore, priceOre)
		player.Score += points
		if distance == 0 {
			player.ExactCount++
		}

		match.RoundPoints[player.Token] = points
		match.RoundDistances[player.Token] = distance
		if closest < 0 || distance < closest {
			closest = distance
		}
	}

	match.ClosestOre = closest
	match.History = append(match.History, stridRoundRecord{
		Round:   match.Round,
		Product: product,
		Guesses: copyStridGuesses(match.Guesses),
		Points:  copyStridPoints(match.RoundPoints),
	})
	l.scheduleLocked(stridRevealDuration)
}

func copyStridGuesses(source map[string]stridGuess) map[string]stridGuess {
	copied := make(map[string]stridGuess, len(source))
	for token, guess := range source {
		copied[token] = guess
	}
	return copied
}

func copyStridPoints(source map[string]int) map[string]int {
	copied := make(map[string]int, len(source))
	for token, points := range source {
		copied[token] = points
	}
	return copied
}

func (l *stridLobby) nextRoundLocked() {
	match := l.match
	if match == nil {
		return
	}

	if match.Round >= stridRounds {
		match.Phase = stridPhaseFinished
		l.status = stridStatusFinished
		return
	}

	match.Round++
	l.beginRoundLocked(time.Now())
}

// stridScore rewards precision. An exact hit on the öre gives all 5000 points,
// and the score falls linearly to zero as the guess moves a full price away
// from the answer. Measuring the distance relative to the price keeps the game
// fair for both a 5 kr item and a 200 kr item.
func stridScore(guessOre, priceOre int) int {
	if priceOre <= 0 {
		return 0
	}

	distance := guessOre - priceOre
	if distance < 0 {
		distance = -distance
	}
	if distance >= priceOre {
		return 0
	}

	return (stridMaxPoints*(priceOre-distance) + priceOre/2) / priceOre
}

func priceToOre(price float64) int {
	return int(math.Round(price * 100))
}

func pickStridProducts(count int) ([]Product, error) {
	productsMu.RLock()
	defer productsMu.RUnlock()

	if len(products) == 0 {
		return nil, fmt.Errorf("product pool is empty")
	}

	picked := make([]Product, 0, count)
	if len(products) >= count {
		// A partial Fisher-Yates shuffle gives unique products for all rounds
		// without copying the whole pool.
		pool := append([]Product(nil), products...)
		for i := 0; i < count; i++ {
			j := i + rand.IntN(len(pool)-i)
			pool[i], pool[j] = pool[j], pool[i]
			picked = append(picked, pool[i])
		}
		return picked, nil
	}

	// A tiny pool is unusual, but repeating the products is still better than
	// refusing to start a match.
	for i := 0; i < count; i++ {
		picked = append(picked, products[rand.IntN(len(products))])
	}
	return picked, nil
}

func newStridCode() (string, error) {
	limit := big.NewInt(int64(len(stridAlphabet)))
	code := make([]byte, stridCodeLength)

	for i := range code {
		index, err := crand.Int(crand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("generate lobby code: %w", err)
		}
		code[i] = stridAlphabet[index.Int64()]
	}
	return string(code), nil
}

func newStridToken() (string, error) {
	token := make([]byte, 24)
	if _, err := crand.Read(token); err != nil {
		return "", fmt.Errorf("generate player token: %w", err)
	}
	return hex.EncodeToString(token), nil
}

func normalizeStridCode(raw string) (string, bool) {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if len(code) != stridCodeLength {
		return "", false
	}
	for i := 0; i < len(code); i++ {
		character := code[i]
		if (character < 'A' || character > 'Z') && (character < '0' || character > '9') {
			return "", false
		}
	}
	return code, true
}

func normalizeStridName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" || utf8.RuneCountInString(name) > stridNameRunes {
		return "", false
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return "", false
		}
	}
	return name, true
}

/* ----------------------------- State ----------------------------- */

type stridState struct {
	Code     string `json:"code"`
	Status   string `json:"status"`
	Phase    string `json:"phase"`
	Revision uint64 `json:"revision"`

	ServerNow int64 `json:"server_now"`

	MinPlayers   int `json:"min_players"`
	MaxPlayers   int `json:"max_players"`
	Rounds       int `json:"rounds"`
	GuessSeconds int `json:"guess_seconds"`

	CanStart bool               `json:"can_start"`
	Players  []stridPlayerState `json:"players"`
	Round    *stridRoundState   `json:"round,omitempty"`
	Winners  []string           `json:"winners,omitempty"`
	Recap    *stridRecap        `json:"recap,omitempty"`
}

// stridRecap is the final table: every round's product, its price and every
// player's guess, with the players in the same order as state.Players.
type stridRecap struct {
	Players []string          `json:"players"`
	Rounds  []stridRecapRound `json:"rounds"`
}

type stridRecapRound struct {
	Number  int           `json:"number"`
	Product publicProduct `json:"product"`
	Price   float64       `json:"price"`
	Guesses []*float64    `json:"guesses"`
	Points  []int         `json:"points"`
}

type stridPlayerState struct {
	Name      string `json:"name,omitempty"`
	Host      bool   `json:"host"`
	Ready     bool   `json:"ready"`
	You       bool   `json:"you,omitempty"`
	Connected bool   `json:"connected"`
	Score     int    `json:"score"`
	Place     int    `json:"place,omitempty"`

	Guessed bool     `json:"guessed"`
	Guess   *float64 `json:"guess,omitempty"`

	Points   *int     `json:"points,omitempty"`
	Distance *float64 `json:"distance,omitempty"`
	Closest  bool     `json:"closest,omitempty"`
	Exact    bool     `json:"exact,omitempty"`
	Winner   bool     `json:"winner,omitempty"`
}

type stridRoundState struct {
	Number  int           `json:"number"`
	Total   int           `json:"total"`
	Product publicProduct `json:"product"`

	EndsAt       int64 `json:"ends_at"`
	RevealEndsAt int64 `json:"reveal_ends_at,omitempty"`

	// Price is only set once the round is over.
	Price *float64 `json:"price,omitempty"`
}

func (l *stridLobby) snapshotLocked(viewer *stridPlayer) stridState {
	now := time.Now()
	state := stridState{
		Code:         l.code,
		Status:       l.status,
		Phase:        stridPhaseLobby,
		Revision:     l.revision,
		ServerNow:    now.UnixMilli(),
		MinPlayers:   stridMinPlayers,
		MaxPlayers:   stridMaxPlayers,
		Rounds:       stridRounds,
		GuessSeconds: stridGuessSeconds,
	}

	match := l.match
	if match != nil {
		switch l.status {
		case stridStatusPlaying:
			state.Phase = match.Phase
		case stridStatusFinished:
			state.Phase = stridPhaseFinished
		}
	}

	places := l.placesLocked()
	winners := l.winnerNamesLocked()
	winnerSet := make(map[string]bool, len(winners))
	for _, name := range winners {
		winnerSet[name] = true
	}
	state.Winners = winners
	if l.status == stridStatusFinished {
		state.Recap = l.recapLocked()
	}

	if viewer != nil && viewer.Host && l.status == stridStatusWaiting &&
		len(l.players) >= stridMinPlayers && len(l.players) <= stridMaxPlayers {
		state.CanStart = true
		for _, player := range l.players {
			if !player.Ready {
				state.CanStart = false
				break
			}
		}
	}

	state.Players = make([]stridPlayerState, 0, len(l.players))
	for _, player := range l.players {
		view := stridPlayerState{
			Name:      player.Name,
			Host:      player.Host,
			Ready:     player.Ready,
			You:       player == viewer,
			Connected: player.connections > 0,
			Score:     player.Score,
			Place:     places[player.Token],
			Winner:    winnerSet[player.Name],
		}

		if match != nil {
			if guess, ok := match.Guesses[player.Token]; ok {
				view.Guessed = true
				// A guess is secret until the round is over, except to the
				// player who made it.
				if player == viewer || match.HasResults {
					value := guess.Value
					view.Guess = &value
				}
			}

			if match.HasResults {
				points := match.RoundPoints[player.Token]
				view.Points = &points

				if distance, ok := match.RoundDistances[player.Token]; ok {
					kronor := float64(distance) / 100
					view.Distance = &kronor
					view.Exact = distance == 0
					view.Closest = match.ClosestOre >= 0 && distance == match.ClosestOre
				}
			}
		}

		state.Players = append(state.Players, view)
	}

	if match != nil && match.Round >= 1 && match.Round <= len(match.Products) {
		product := match.Products[match.Round-1]
		round := &stridRoundState{
			Number:  match.Round,
			Total:   stridRounds,
			Product: toPublicProduct(product),
			EndsAt:  match.RoundEndsAt.UnixMilli(),
		}
		if !match.RevealEndsAt.IsZero() {
			round.RevealEndsAt = match.RevealEndsAt.UnixMilli()
		}
		if match.HasResults || match.Phase == stridPhaseFinished {
			price := product.Price
			round.Price = &price
		}
		state.Round = round
	}

	return state
}

// recapLocked builds the final table from the closed rounds. Guesses and
// points are aligned with the player order used by state.Players.
func (l *stridLobby) recapLocked() *stridRecap {
	match := l.match
	if match == nil || len(match.History) == 0 {
		return nil
	}

	players := make([]string, len(l.players))
	for index, player := range l.players {
		players[index] = player.Name
	}

	rounds := make([]stridRecapRound, 0, len(match.History))
	for _, record := range match.History {
		round := stridRecapRound{
			Number:  record.Round,
			Product: toPublicProduct(record.Product),
			Price:   record.Product.Price,
			Guesses: make([]*float64, len(l.players)),
			Points:  make([]int, len(l.players)),
		}

		for index, player := range l.players {
			if guess, ok := record.Guesses[player.Token]; ok {
				value := guess.Value
				round.Guesses[index] = &value
			}
			round.Points[index] = record.Points[player.Token]
		}

		rounds = append(rounds, round)
	}

	return &stridRecap{Players: players, Rounds: rounds}
}

// placesLocked ranks players by total score. Players on the same score share a
// place, which matches the rule that the highest score wins.
func (l *stridLobby) placesLocked() map[string]int {
	places := make(map[string]int, len(l.players))
	if l.status == stridStatusWaiting {
		return places
	}

	sorted := append([]*stridPlayer(nil), l.players...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Score != sorted[j].Score {
			return sorted[i].Score > sorted[j].Score
		}
		return strings.ToLower(sorted[i].Name) < strings.ToLower(sorted[j].Name)
	})

	place := 1
	for index, player := range sorted {
		if index > 0 && player.Score != sorted[index-1].Score {
			place = index + 1
		}
		places[player.Token] = place
	}
	return places
}

func (l *stridLobby) winnerNamesLocked() []string {
	if l.status != stridStatusFinished || len(l.players) == 0 {
		return nil
	}

	best := l.players[0].Score
	for _, player := range l.players[1:] {
		if player.Score > best {
			best = player.Score
		}
	}

	names := make([]string, 0, len(l.players))
	for _, player := range l.players {
		if player.Score == best && player.Name != "" {
			names = append(names, player.Name)
		}
	}
	return names
}
