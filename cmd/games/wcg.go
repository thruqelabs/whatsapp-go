// Package utils provides the core Word Chain Game (WCG) engine.
package games

import (
	"context"
	crand "crypto/rand"
	"math/big"
	"math/rand"
	"strings"
	"sync"
	"time"
	"unicode"
	"whatsrook/util/logger"

	"whatsrook"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// GetRandomStartingLetter returns a cryptographically random uppercase letter from A to Z with uniform distribution.
func GetRandomStartingLetter() rune {
	letters := []rune("ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	n, err := crand.Int(crand.Reader, big.NewInt(int64(len(letters))))
	if err != nil {
		return letters[rand.Intn(len(letters))]
	}
	return letters[n.Int64()]
}

type WCGState int

const (
	WCGStateLobby WCGState = iota
	WCGStateInProgress
)

type WCGPlayer struct {
	LID            types.JID
	MentionJID     types.JID
	Tag            string
	Score          int
	Eliminated     bool
	CorrectGuesses int
	TotalTimeMs    int64
	GuessesCount   int
	JoinedAt       time.Time
}

type WCGGame struct {
	Mu                sync.Mutex
	ChatKey           string
	State             WCGState
	HostLID           types.JID
	HostMention       types.JID
	HostTag           string
	Players           []*WCGPlayer
	CurrentTurnIdx    int
	RequiredChar      rune
	LetterRepeatCount int
	MinLength         int
	RoundCount        int
	AnswersInRound    int
	UsedWords         map[string]bool
	TurnStartTime     time.Time
	LobbyTimer        *time.Timer
	TurnTimer         *time.Timer
	GameStartTime     time.Time
	Client            *whatsmeow.Client
	ChatJID           types.JID
}

var (
	wcgMu    sync.Mutex
	wcgGames = make(map[string]*WCGGame) // chat key -> game
)

// GetWCGGame returns the active game for a chat key, or nil.
func GetWCGGame(chatKey string) *WCGGame {
	wcgMu.Lock()
	defer wcgMu.Unlock()
	return wcgGames[chatKey]
}

// CreateWCGGame creates a new lobby for a chat.
func CreateWCGGame(chatKey string, hostLID, hostMention types.JID, hostTag string, chatJID types.JID, client *whatsmeow.Client) *WCGGame {
	game := &WCGGame{
		ChatKey:      chatKey,
		State:        WCGStateLobby,
		HostLID:      hostLID,
		HostMention:  hostMention,
		HostTag:      hostTag,
		RequiredChar: GetRandomStartingLetter(),
		MinLength:    3,
		RoundCount:   1,
		UsedWords:    make(map[string]bool),
		ChatJID:      chatJID,
		Client:       client,
	}

	game.Players = append(game.Players, &WCGPlayer{
		LID:        hostLID,
		MentionJID: hostMention,
		Tag:        hostTag,
		JoinedAt:   time.Now(),
	})

	wcgMu.Lock()
	wcgGames[chatKey] = game
	wcgMu.Unlock()

	return game
}

// DeleteWCGGame removes a game from the active map.
func DeleteWCGGame(chatKey string) {
	wcgMu.Lock()
	defer wcgMu.Unlock()
	delete(wcgGames, chatKey)
}

// AddPlayer adds a player to an existing lobby.
func (g *WCGGame) AddPlayer(lid, mentionJID types.JID, tag string) bool {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	if g.State != WCGStateLobby {
		return false
	}
	if g.FindPlayerIndex(lid) != -1 {
		return false
	}

	g.Players = append(g.Players, &WCGPlayer{
		LID:        lid,
		MentionJID: mentionJID,
		Tag:        tag,
		JoinedAt:   time.Now(),
	})
	return true
}

// IsHost returns true if the specified user JID is the game host/initiator.
func (g *WCGGame) IsHost(user types.JID) bool {
	u := user.ToNonAD()
	hLID := g.HostLID.ToNonAD()
	hMen := g.HostMention.ToNonAD()

	if !hLID.IsEmpty() && hLID == u {
		logger.Debug("[WCG IsHost] direct match on HostLID", "user", u.String(), "hostLID", hLID.String())
		return true
	}
	if !hMen.IsEmpty() && hMen == u {
		logger.Debug("[WCG IsHost] direct match on HostMention", "user", u.String(), "hostMention", hMen.String())
		return true
	}
	if !hLID.IsEmpty() && hLID.Server == u.Server && hLID.User == u.User {
		logger.Debug("[WCG IsHost] same-server match on HostLID", "user", u.String(), "hostLID", hLID.String())
		return true
	}
	if !hMen.IsEmpty() && hMen.Server == u.Server && hMen.User == u.User {
		logger.Debug("[WCG IsHost] same-server match on HostMention", "user", u.String(), "hostMention", hMen.String())
		return true
	}
	if g.Client != nil {
		if !g.HostLID.IsEmpty() && whatsrook.IsSameUserRaw(context.Background(), g.Client, g.HostLID, u) {
			logger.Debug("[WCG IsHost] IsSameUserRaw match on HostLID", "user", u.String(), "hostLID", g.HostLID.String())
			return true
		}
		if !g.HostMention.IsEmpty() && whatsrook.IsSameUserRaw(context.Background(), g.Client, g.HostMention, u) {
			logger.Debug("[WCG IsHost] IsSameUserRaw match on HostMention", "user", u.String(), "hostMention", g.HostMention.String())
			return true
		}
	}
	logger.Debug("[WCG IsHost] check failed (not host)", "user", u.String(), "hostLID", hLID.String(), "hostMention", hMen.String())
	return false
}

// FindPlayerIndex returns the index of a player by LID, MentionJID, or matching user, or -1.
func (g *WCGGame) FindPlayerIndex(user types.JID) int {
	u := user.ToNonAD()
	for i, p := range g.Players {
		pLID := p.LID.ToNonAD()
		pMen := p.MentionJID.ToNonAD()
		if pLID == u || pMen == u || (pLID.User != "" && pLID.User == u.User) || (pMen.User != "" && pMen.User == u.User) {
			return i
		}
		if g.Client != nil {
			if whatsrook.IsSameUserRaw(context.Background(), g.Client, p.LID, u) || whatsrook.IsSameUserRaw(context.Background(), g.Client, p.MentionJID, u) {
				return i
			}
		}
	}
	return -1
}

// IsPlayerEliminated returns true if the player with given LID is eliminated or not found in the match.
func (g *WCGGame) IsPlayerEliminated(lid types.JID) bool {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	idx := g.FindPlayerIndex(lid)
	if idx == -1 {
		return true
	}
	return g.Players[idx].Eliminated
}

// GetActivePlayers returns all non-eliminated players.
func (g *WCGGame) GetActivePlayers() []*WCGPlayer {
	var active []*WCGPlayer
	for _, p := range g.Players {
		if !p.Eliminated {
			active = append(active, p)
		}
	}
	return active
}

// IsWordUsed checks if a word has already been submitted in the current game.
func (g *WCGGame) IsWordUsed(word string) bool {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	return g.UsedWords[strings.ToLower(word)]
}

// StartGame transitions the game from lobby to in-progress.
func (g *WCGGame) StartGame() bool {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	if g.State == WCGStateInProgress {
		return false
	}

	g.State = WCGStateInProgress
	g.GameStartTime = time.Now()
	g.RequiredChar = GetRandomStartingLetter()
	g.MinLength = 3
	g.RoundCount = 1
	g.AnswersInRound = 0
	g.CurrentTurnIdx = 0
	g.UsedWords = make(map[string]bool)

	active := g.GetActivePlayers()
	if len(active) == 0 {
		DeleteWCGGame(g.ChatKey)
		return false
	}

	return true
}

// StartTurn sets up turn parameters for current player.
func (g *WCGGame) StartTurn() (reqChar rune, minLen int, timeLimitSec int, currentPlayer *WCGPlayer) {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	active := g.GetActivePlayers()
	if len(active) == 0 {
		return 'A', 3, 0, nil
	}

	if g.CurrentTurnIdx >= len(g.Players) {
		g.CurrentTurnIdx = 0
	}
	for g.Players[g.CurrentTurnIdx].Eliminated {
		g.CurrentTurnIdx = (g.CurrentTurnIdx + 1) % len(g.Players)
	}

	currentPlayer = g.Players[g.CurrentTurnIdx]
	g.TurnStartTime = time.Now()

	// Dynamic time limit: Round 1 = 25s, decreasing by 2s per round down to minimum of 6s
	timeLimitSec = max(25-(g.RoundCount-1)*2, 6)

	return g.RequiredChar, g.MinLength, timeLimitSec, currentPlayer
}

// ProcessGuess processes a valid word submission.
func (g *WCGGame) ProcessGuess(word string, senderLID types.JID) (correct bool, gameOver bool, winner *WCGPlayer, currentPlayer *WCGPlayer, elapsed time.Duration) {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	if g.State != WCGStateInProgress {
		return false, true, nil, nil, 0
	}

	pIdx := g.FindPlayerIndex(senderLID)
	if pIdx == -1 {
		return false, false, nil, nil, 0
	}

	active := g.GetActivePlayers()
	if len(active) == 0 {
		return false, true, nil, nil, 0
	}

	currentPlayer = g.Players[g.CurrentTurnIdx]
	if pIdx != g.CurrentTurnIdx {
		return false, false, nil, currentPlayer, 0
	}

	word = strings.ToLower(strings.TrimSpace(word))
	elapsed = time.Since(g.TurnStartTime)

	if g.TurnTimer != nil {
		g.TurnTimer.Stop()
	}

	currentPlayer.GuessesCount++
	currentPlayer.TotalTimeMs += elapsed.Milliseconds()

	// Record used word & score
	g.UsedWords[word] = true
	currentPlayer.Score += len(word) * 10
	currentPlayer.CorrectGuesses++

	// Next required starting character is the last character of the submitted word (in uppercase)
	nextChar := unicode.ToUpper(rune(word[len(word)-1]))
	if nextChar == g.RequiredChar {
		g.LetterRepeatCount++
	} else {
		g.LetterRepeatCount = 0
	}

	// Break repetitive letter loops (e.g. L -> L -> L or Y -> Y -> Y)
	if g.LetterRepeatCount >= 2 {
		g.RequiredChar = GetRandomStartingLetter()
		g.LetterRepeatCount = 0
	} else {
		g.RequiredChar = nextChar
	}

	g.AnswersInRound++
	activeCount := len(g.GetActivePlayers())
	// After all active players have answered in the current round, increase word length and start next round with a fresh letter
	if g.AnswersInRound >= activeCount {
		g.AnswersInRound = 0
		g.RoundCount++
		g.MinLength++
		g.RequiredChar = GetRandomStartingLetter()
		g.LetterRepeatCount = 0
	}

	g.advanceTurnUnsafe()
	return true, false, nil, currentPlayer, elapsed
}

func (g *WCGGame) advanceTurnUnsafe() {
	g.CurrentTurnIdx = (g.CurrentTurnIdx + 1) % len(g.Players)
	for g.Players[g.CurrentTurnIdx].Eliminated {
		g.CurrentTurnIdx = (g.CurrentTurnIdx + 1) % len(g.Players)
	}
}

// EliminateCurrentPlayer eliminates the current turn player. Returns gameOver bool when 1 or 0 active players remain.
func (g *WCGGame) EliminateCurrentPlayer() (gameOver bool, winner *WCGPlayer) {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	if g.CurrentTurnIdx < len(g.Players) {
		g.Players[g.CurrentTurnIdx].Eliminated = true
	}

	rem := g.GetActivePlayers()
	if len(rem) <= 1 {
		if len(rem) == 1 {
			winner = rem[0]
		}
		return true, winner
	}

	activeCount := len(rem)
	if g.AnswersInRound >= activeCount {
		g.AnswersInRound = 0
		g.RoundCount++
		g.MinLength++
	}

	g.advanceTurnUnsafe()
	return false, nil
}

// FinishGame cleans up the game and returns final standings.
func (g *WCGGame) FinishGame() (winner *WCGPlayer, standings []*WCGPlayer) {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	if g.TurnTimer != nil {
		g.TurnTimer.Stop()
	}

	active := g.GetActivePlayers()
	if len(active) == 1 {
		winner = active[0]
	}

	standings = make([]*WCGPlayer, len(g.Players))
	copy(standings, g.Players)
	for i := 0; i < len(standings); i++ {
		for j := i + 1; j < len(standings); j++ {
			if standings[j].Score > standings[i].Score {
				standings[i], standings[j] = standings[j], standings[i]
			}
		}
	}

	DeleteWCGGame(g.ChatKey)
	return winner, standings
}

// GetSortedPlayers returns players sorted by score descending.
func (g *WCGGame) GetSortedPlayers() []*WCGPlayer {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	sorted := make([]*WCGPlayer, len(g.Players))
	copy(sorted, g.Players)
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].Score > sorted[i].Score {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	return sorted
}

func (g *WCGGame) SetLobbyTimer(timer *time.Timer) {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	g.LobbyTimer = timer
}

func (g *WCGGame) SetTurnTimer(timer *time.Timer) {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	g.TurnTimer = timer
}

func (g *WCGGame) StopTimers() {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	if g.LobbyTimer != nil {
		g.LobbyTimer.Stop()
	}
	if g.TurnTimer != nil {
		g.TurnTimer.Stop()
	}
}
