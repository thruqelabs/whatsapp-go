package games

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"whatsrook"
	"whatsrook/util/httpx"
	"whatsrook/util/logger"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

type UnscrambleState int

const (
	UnscrambleStateLobby UnscrambleState = iota
	UnscrambleStateInProgress
)

type UnscramblePlayer struct {
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

type UnscrambleGame struct {
	Mu             sync.Mutex
	ChatKey        string
	State          UnscrambleState
	HostLID        types.JID
	HostMention    types.JID
	HostTag        string
	Players        []*UnscramblePlayer
	CurrentTurnIdx int
	WordLength     int // 3 to 16
	CurrentWord    string
	ScrambledWord  string
	CurrentHint    string
	TurnStartTime  time.Time
	LobbyTimer     *time.Timer
	TurnTimer      *time.Timer
	GameStartTime  time.Time
	Client         *whatsmeow.Client
	ChatJID        types.JID
}

var (
	unscrambleMu    sync.Mutex
	unscrambleGames = make(map[string]*UnscrambleGame) // chat key -> game

	defCacheMu sync.RWMutex
	defCache   = make(map[string]string)
)

// wordList holds validated English words categorized strictly by character length (3-16 letters).
var wordList = map[int][]string{
	3: {
		"cat", "dog", "bat", "rat", "hat", "sun", "run", "fun", "pen", "cup",
		"box", "fox", "map", "tap", "nap", "lip", "hip", "rib", "web", "mud",
		"sky", "ice", "bed", "owl", "bus", "key", "ant", "fly", "arm", "art",
	},
	4: {
		"bird", "fish", "tree", "book", "desk", "lamp", "door", "road", "star", "moon",
		"hand", "foot", "head", "face", "time", "love", "hope", "fire", "wind", "rain",
		"gold", "ring", "snow", "king", "duck", "bear", "lion", "frog", "leaf", "rose",
	},
	5: {
		"apple", "grape", "lemon", "mango", "peach", "chair", "table", "house", "water", "earth",
		"music", "dance", "smile", "laugh", "dream", "light", "night", "world", "peace", "power",
		"beach", "cloud", "tiger", "train", "bread", "clock", "river", "robot", "snake", "plant",
	},
	6: {
		"banana", "orange", "cherry", "rabbit", "turtle", "window", "garden", "forest", "bridge", "castle",
		"island", "desert", "planet", "rocket", "puzzle", "secret", "winter", "summer", "spring", "autumn",
		"guitar", "doctor", "laptop", "spider", "coffee", "dragon", "engine", "flower", "mirror", "silver",
	},
	7: {
		"dolphin", "penguin", "giraffe", "leopard", "kitchen", "bedroom", "balcony", "station", "journey", "mystery",
		"history", "science", "fiction", "fantasy", "silence", "thunder", "rainbow", "sunrise", "diamond", "feather",
		"octopus", "pyramid", "volcano", "lantern", "captain", "blanket", "crystal", "monster", "package", "whisper",
	},
	8: {
		"dinosaur", "elephant", "kangaroo", "tomorrow", "mountain", "sapphire", "midnight", "twilight", "daylight", "sunshine",
		"flamingo", "football", "hospital", "notebook", "painting", "umbrella", "universe", "computer", "sandwich", "building",
		"calendar", "fountain", "squirrel", "triangle", "guardian", "treasure", "wildlife", "velocity", "wanderer", "blizzard",
	},
	9: {
		"crocodile", "alligator", "chameleon", "butterfly", "waterfall", "landscape", "adventure", "discovery", "knowledge", "happiness",
		"astronaut", "chocolate", "dandelion", "harmonica", "jellyfish", "pineapple", "spaceship", "submarine", "sunflower", "telescope",
		"sanctuary", "orchestra", "dragonfly", "labyrinth", "greatness", "porcupine", "saxophone", "boulevard", "swordfish", "quicksand",
	},
	10: {
		"chimpanzee", "salamander", "watermelon", "strawberry", "blackberry", "television", "microscope", "laboratory", "university", "dictionary",
		"vocabulary", "literature", "philosophy", "blacksmith", "earthquake", "helicopter", "locomotive", "rainforest", "volleyball", "gymnastics",
		"rhinoceros", "friendship", "leadership", "basketball", "skateboard", "playground", "motorcycle", "trampoline", "underwater", "cheesecake",
	},
	11: {
		"caterpillar", "grasshopper", "destination", "imagination", "information", "combination", "celebration", "observation", "examination", "explanation",
		"application", "development", "environment", "electricity", "masterpiece", "supermarket", "engineering", "photography", "composition", "mathematics",
		"dragonflies", "woodpeckers", "skateboards", "microphones", "perspective", "performance", "camaraderie", "aeronautics", "corporation", "independent",
	},
	12: {
		"hippopotamus", "biographical", "trigonometry", "microbiology", "astrophysics", "oceanography", "anthropology", "psychiatrist", "cardiologist", "pediatrician",
		"veterinarian", "nutritionist", "chiropractor", "architecture", "biodiversity", "conglomerate", "constitution", "illumination", "intelligence", "relationship",
		"kindergarten", "organization", "photographer", "refrigerator", "neighborhood", "proclamation", "thunderstorm", "marshmallows", "snowboarding", "cheeseburger",
		"authenticity", "breathtaking",
	},
	13: {
		"parallelogram", "biotechnology", "communication", "demonstration", "investigation", "determination", "participation", "consideration", "establishment", "recombination",
		"unpredictable", "archaeologist", "autobiography", "extravaganzas", "hallucination", "understanding", "environmental", "qualification", "vulnerability", "electrostatic",
		"cheeseburgers", "constellation", "disappearance", "individualism", "entertainment", "telemarketing", "jurisdictions", "microorganism", "civilizations", "comprehension",
	},
	14: {
		"accomplishment", "administration", "classification", "discrimination", "infrastructure", "interpretation", "microorganisms", "predictability", "reorganization", "representation",
		"responsibility", "recommendation", "implementation", "rehabilitation", "accountability", "individualisms", "characterizing", "decentralizing", "counterattacks", "transformation",
		"transportation", "disappointment", "identification", "embarrassments", "comprehensions", "generalization", "neutralization", "specialization", "reconstruction", "discontentment",
	},
	15: {
		"congratulations", "characteristics", "confidentiality", "experimentation", "extraordinarily", "instrumentation", "interchangeable", "personification", "proportionality", "rationalization",
		"synchronization", "troubleshooting", "professionalism", "instrumentality", "standardisation", "standardization", "discontinuation", "identifications", "implementations", "transformations",
		"differentiation", "exemplification", "hospitalization", "interconnection", "notwithstanding", "photosynthesize", "straightforward", "reorganizations", "representations",
	},
	16: {
		"characterization", "incomprehensible", "intercontinental", "internationalize", "miscommunication", "unconstitutional", "unpredictability", "compartmentalize", "counteroffensive", "institutionalize",
		"representational", "telecommunicator", "electrochemistry", "electromagnetism", "overcompensation", "photolithography", "multidimensional", "telecommunicates", "photosynthesizer", "compartmentalise",
		"responsibilities",
	},
}

// GetUnscrambleGame returns the active game for a chat key, or nil.
func GetUnscrambleGame(chatKey string) *UnscrambleGame {
	unscrambleMu.Lock()
	defer unscrambleMu.Unlock()
	return unscrambleGames[chatKey]
}

// CreateUnscrambleGame creates a new lobby for a chat.
func CreateUnscrambleGame(chatKey string, hostLID, hostMention types.JID, hostTag string, chatJID types.JID, client *whatsmeow.Client) *UnscrambleGame {
	game := &UnscrambleGame{
		ChatKey:     chatKey,
		State:       UnscrambleStateLobby,
		HostLID:     hostLID,
		HostMention: hostMention,
		HostTag:     hostTag,
		WordLength:  3,
		ChatJID:     chatJID,
		Client:      client,
	}

	// Host automatically joins
	game.Players = append(game.Players, &UnscramblePlayer{
		LID:        hostLID,
		MentionJID: hostMention,
		Tag:        hostTag,
		JoinedAt:   time.Now(),
	})

	unscrambleMu.Lock()
	unscrambleGames[chatKey] = game
	unscrambleMu.Unlock()

	return game
}

// IsHost returns true if the specified user JID is the game host/initiator.
func (g *UnscrambleGame) IsHost(user types.JID) bool {
	u := user.ToNonAD()
	hLID := g.HostLID.ToNonAD()
	hMen := g.HostMention.ToNonAD()

	if !hLID.IsEmpty() && hLID == u {
		logger.Debug("[Unscramble IsHost] direct match on HostLID", "user", u.String(), "hostLID", hLID.String())
		return true
	}
	if !hMen.IsEmpty() && hMen == u {
		logger.Debug("[Unscramble IsHost] direct match on HostMention", "user", u.String(), "hostMention", hMen.String())
		return true
	}
	if !hLID.IsEmpty() && hLID.Server == u.Server && hLID.User == u.User {
		logger.Debug("[Unscramble IsHost] same-server match on HostLID", "user", u.String(), "hostLID", hLID.String())
		return true
	}
	if !hMen.IsEmpty() && hMen.Server == u.Server && hMen.User == u.User {
		logger.Debug("[Unscramble IsHost] same-server match on HostMention", "user", u.String(), "hostMention", hMen.String())
		return true
	}
	if g.Client != nil {
		if !g.HostLID.IsEmpty() && whatsrook.IsSameUserRaw(context.Background(), g.Client, g.HostLID, u) {
			logger.Debug("[Unscramble IsHost] IsSameUserRaw match on HostLID", "user", u.String(), "hostLID", g.HostLID.String())
			return true
		}
		if !g.HostMention.IsEmpty() && whatsrook.IsSameUserRaw(context.Background(), g.Client, g.HostMention, u) {
			logger.Debug("[Unscramble IsHost] IsSameUserRaw match on HostMention", "user", u.String(), "hostMention", g.HostMention.String())
			return true
		}
	}
	logger.Debug("[Unscramble IsHost] check failed (not host)", "user", u.String(), "hostLID", hLID.String(), "hostMention", hMen.String())
	return false
}

// DeleteUnscrambleGame removes a game from the active map.
func DeleteUnscrambleGame(chatKey string) {
	unscrambleMu.Lock()
	defer unscrambleMu.Unlock()
	delete(unscrambleGames, chatKey)
}

// AddPlayer adds a player to an existing lobby.
func (g *UnscrambleGame) AddPlayer(lid, mentionJID types.JID, tag string) bool {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	if g.State != UnscrambleStateLobby {
		return false
	}
	if g.FindPlayerIndex(lid) != -1 {
		return false
	}

	g.Players = append(g.Players, &UnscramblePlayer{
		LID:        lid,
		MentionJID: mentionJID,
		Tag:        tag,
		JoinedAt:   time.Now(),
	})
	return true
}

// FindPlayerIndex returns the index of a player by LID, MentionJID, or matching user, or -1.
func (g *UnscrambleGame) FindPlayerIndex(user types.JID) int {
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

// GetActivePlayers returns all non-eliminated players.
func (g *UnscrambleGame) GetActivePlayers() []*UnscramblePlayer {
	var active []*UnscramblePlayer
	for _, p := range g.Players {
		if !p.Eliminated {
			active = append(active, p)
		}
	}
	return active
}

// StartGame transitions the game from lobby to in-progress.
func (g *UnscrambleGame) StartGame() bool {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	if g.State == UnscrambleStateInProgress {
		return false
	}

	g.State = UnscrambleStateInProgress
	g.GameStartTime = time.Now()
	g.WordLength = 3
	g.CurrentTurnIdx = 0

	active := g.GetActivePlayers()
	if len(active) == 0 {
		DeleteUnscrambleGame(g.ChatKey)
		return false
	}

	return true
}

// StartTurn sets up a new turn with a scrambled word and definition hint.
func (g *UnscrambleGame) StartTurn() (scrambled string, hint string, timeLimitSec int, currentPlayer *UnscramblePlayer) {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	active := g.GetActivePlayers()
	if len(active) == 0 {
		return "", "", 0, nil
	}

	// Ensure currentTurnIdx points to valid active player
	if g.CurrentTurnIdx >= len(g.Players) {
		g.CurrentTurnIdx = 0
	}
	for g.Players[g.CurrentTurnIdx].Eliminated {
		g.CurrentTurnIdx = (g.CurrentTurnIdx + 1) % len(g.Players)
	}

	currentPlayer = g.Players[g.CurrentTurnIdx]

	word, scrambled, hint := GetRandomWordWithHint(g.WordLength)
	g.CurrentWord = word
	g.ScrambledWord = scrambled
	g.CurrentHint = hint
	g.TurnStartTime = time.Now()

	timeLimitSec = GetTurnTimeLimit(g.WordLength)

	return scrambled, hint, timeLimitSec, currentPlayer
}

// ProcessGuess handles a player's guess. Returns: correct bool, gameOver bool, winner *UnscramblePlayer.
func (g *UnscrambleGame) ProcessGuess(guess string, senderLID types.JID) (correct bool, gameOver bool, winner *UnscramblePlayer, currentPlayer *UnscramblePlayer, elapsed time.Duration) {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	if g.State != UnscrambleStateInProgress {
		return false, true, nil, nil, 0
	}

	// Check if sender is in game and is current turn
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

	guess = strings.ToLower(strings.TrimSpace(guess))
	elapsed = time.Since(g.TurnStartTime)

	if g.TurnTimer != nil {
		g.TurnTimer.Stop()
	}

	currentPlayer.GuessesCount++
	currentPlayer.TotalTimeMs += elapsed.Milliseconds()

	if guess == g.CurrentWord {
		// Correct!
		currentPlayer.Score += g.WordLength * 10
		currentPlayer.CorrectGuesses++

		if g.WordLength < 16 {
			g.WordLength++
		} else {
			// Reached max level - single player wins, or multiplayer continues until someone fails
			// For single player, this is a win condition
			rem := g.GetActivePlayers()
			if len(rem) == 1 {
				return true, true, rem[0], currentPlayer, elapsed
			}
		}

		g.advanceTurnUnsafe()
		return true, false, nil, currentPlayer, elapsed
	}

	// Wrong guess - eliminate player
	currentPlayer.Eliminated = true

	rem := g.GetActivePlayers()
	if len(rem) <= 1 {
		if len(rem) == 1 {
			winner = rem[0]
		}
		return false, true, winner, currentPlayer, elapsed
	}

	g.advanceTurnUnsafe()
	return false, false, nil, currentPlayer, elapsed
}

// advanceTurnUnsafe advances to next non-eliminated player. Must hold g.Mu.
func (g *UnscrambleGame) advanceTurnUnsafe() {
	g.CurrentTurnIdx = (g.CurrentTurnIdx + 1) % len(g.Players)
	for g.Players[g.CurrentTurnIdx].Eliminated {
		g.CurrentTurnIdx = (g.CurrentTurnIdx + 1) % len(g.Players)
	}
}

// EliminateCurrentPlayer eliminates the current turn player and advances.
func (g *UnscrambleGame) EliminateCurrentPlayer() (gameOver bool, winner *UnscramblePlayer) {
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

	g.advanceTurnUnsafe()
	return false, nil
}

// FinishGame cleans up the game and returns final standings.
func (g *UnscrambleGame) FinishGame() (winner *UnscramblePlayer, standings []*UnscramblePlayer) {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	if g.TurnTimer != nil {
		g.TurnTimer.Stop()
	}

	active := g.GetActivePlayers()
	if len(active) == 1 {
		winner = active[0]
	}

	// Sort by score descending
	standings = make([]*UnscramblePlayer, len(g.Players))
	copy(standings, g.Players)
	for i := 0; i < len(standings); i++ {
		for j := i + 1; j < len(standings); j++ {
			if standings[j].Score > standings[i].Score {
				standings[i], standings[j] = standings[j], standings[i]
			}
		}
	}

	DeleteUnscrambleGame(g.ChatKey)
	return winner, standings
}

// GetSortedPlayers returns players sorted by score descending.
func (g *UnscrambleGame) GetSortedPlayers() []*UnscramblePlayer {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	sorted := make([]*UnscramblePlayer, len(g.Players))
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

// SetLobbyTimer sets the lobby countdown timer.
func (g *UnscrambleGame) SetLobbyTimer(timer *time.Timer) {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	g.LobbyTimer = timer
}

// SetTurnTimer sets the turn countdown timer.
func (g *UnscrambleGame) SetTurnTimer(timer *time.Timer) {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	g.TurnTimer = timer
}

// StopTimers stops both lobby and turn timers.
func (g *UnscrambleGame) StopTimers() {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	if g.LobbyTimer != nil {
		g.LobbyTimer.Stop()
	}
	if g.TurnTimer != nil {
		g.TurnTimer.Stop()
	}
}

// GetTurnTimeLimit calculates turn duration: Level 3 (3-letter) = 30s, Level 16 (16-letter) = 6s.
func GetTurnTimeLimit(level int) int {
	if level <= 3 {
		return 30
	}
	if level >= 16 {
		return 6
	}
	// Linear interpolation: level 3 -> 30s, level 16 -> 6s
	t := 30 - int(float64(level-3)*(24.0/13.0))
	if t < 6 {
		return 6
	}
	if t > 30 {
		return 30
	}
	return t
}

// GetRandomWordWithHint returns a random word of the given length, its scrambled version, and a dictionary meaning hint.
func GetRandomWordWithHint(length int) (original string, scrambled string, hint string) {
	words, ok := wordList[length]
	if !ok || len(words) == 0 {
		// Fallback: generate random letters
		tb := whatsrook.NewText()
		for range length {
			_ = tb.WriteByte(byte('a' + rand.Intn(26)))
		}
		original = tb.String()
		scrambled = scrambleString(original)
		hint = whatsrook.Sprintf("A %d-letter word", length)
		return original, scrambled, hint
	}

	original = words[rand.Intn(len(words))]
	scrambled = scrambleString(original)
	hint = FetchWordMeaning(original)
	if hint == "" {
		hint = whatsrook.Sprintf("A %d-letter English word", length)
	}
	return original, scrambled, hint
}

// DictionaryDBURL points to the pre-built SQLite English dictionary database.
const DictionaryDBURL = "https://github.com/thruqe/English-Dictionary-SQLite/raw/refs/heads/master/Dictionary.db"

var (
	dictionaryDB   *sql.DB
	dictionaryDBMu sync.RWMutex
)

// ResourceDownloadClient is used for downloading larger game resource files.
var ResourceDownloadClient = httpx.NewClient(5 * time.Minute)

// IsDictionaryDBReady checks if the Dictionary.db file exists and is valid.
func IsDictionaryDBReady() bool {
	dbPath := getDictionaryDBPath()
	info, err := os.Stat(dbPath)
	if err != nil || info.Size() < 10*1024*1024 {
		return false
	}
	return true
}

// ProgressReader wraps an io.Reader and reports progress via a callback.
type ProgressReader struct {
	reader     io.Reader
	total      int64
	downloaded int64
	onProgress func(downloaded, total int64)
}

func (pr *ProgressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	if n > 0 {
		pr.downloaded += int64(n)
		if pr.onProgress != nil {
			pr.onProgress(pr.downloaded, pr.total)
		}
	}
	return n, err
}

// DownloadDictionaryDBWithProgress downloads the SQLite database and periodically reports progress.
func DownloadDictionaryDBWithProgress(ctx context.Context, onProgress func(downloaded, total int64)) error {
	targetPath := getDictionaryDBPath()
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return err
	}

	tempPath := targetPath + ".tmp"
	_ = os.Remove(tempPath)
	defer os.Remove(tempPath)

	if ctx == nil {
		ctx = context.Background()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, DictionaryDBURL, nil)
	if err != nil {
		return fmt.Errorf("failed to build download request: %w", err)
	}
	req.Header.Set("User-Agent", "WhatsRook/1.0 (Dictionary Downloader)")

	resp, err := ResourceDownloadClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch dictionary database: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download server returned HTTP %d", resp.StatusCode)
	}

	out, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}

	pr := &ProgressReader{
		reader:     resp.Body,
		total:      resp.ContentLength,
		onProgress: onProgress,
	}

	_, err = io.Copy(out, pr)
	_ = out.Close()
	if err != nil {
		return fmt.Errorf("failed to write dictionary database: %w", err)
	}

	if err := os.Rename(tempPath, targetPath); err != nil {
		return fmt.Errorf("failed to finalize dictionary database: %w", err)
	}

	// Reset cached connection so it re-opens new DB
	dictionaryDBMu.Lock()
	if dictionaryDB != nil {
		_ = dictionaryDB.Close()
		dictionaryDB = nil
	}
	dictionaryDBMu.Unlock()

	// Warm up connection and build index
	_, _ = getDictionaryDB()
	return nil
}

// DownloadDictionaryDB downloads the English Dictionary SQLite database into the bin folder without callback.
func DownloadDictionaryDB(ctx context.Context) error {
	return DownloadDictionaryDBWithProgress(ctx, nil)
}

func getDictionaryDBPath() string {
	candidates := []string{
		"bin/Dictionary.db",
		"../bin/Dictionary.db",
		"Dictionary.db",
	}

	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(exeDir, "Dictionary.db"),
			filepath.Join(exeDir, "bin", "Dictionary.db"),
			filepath.Join(exeDir, "..", "bin", "Dictionary.db"),
		)
	}

	for _, cand := range candidates {
		if info, err := os.Stat(cand); err == nil && info.Size() >= 10*1024*1024 {
			return cand
		}
	}

	// Default target path for downloads
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		if filepath.Base(exeDir) == "bin" {
			return filepath.Join(exeDir, "Dictionary.db")
		}
	}
	if info, err := os.Stat("../bin"); err == nil && info.IsDir() {
		return filepath.Join("..", "bin", "Dictionary.db")
	}
	return filepath.Join("bin", "Dictionary.db")
}

func getDictionaryDB() (*sql.DB, error) {
	dictionaryDBMu.RLock()
	db := dictionaryDB
	dictionaryDBMu.RUnlock()
	if db != nil {
		return db, nil
	}

	dictionaryDBMu.Lock()
	defer dictionaryDBMu.Unlock()
	if dictionaryDB != nil {
		return dictionaryDB, nil
	}

	dbPath := getDictionaryDBPath()
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}

	conn, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(5)
	conn.SetMaxIdleConns(2)

	// Ensure index exists for fast lookups
	_, _ = conn.Exec("CREATE INDEX IF NOT EXISTS idx_entries_word ON entries(word);")

	dictionaryDB = conn
	return dictionaryDB, nil
}

// FetchWordMeaning retrieves the meaning/definition of a word using local SQLite Dictionary.db with fallbacks.
func FetchWordMeaning(word string) string {
	word = strings.ToLower(strings.TrimSpace(word))
	if word == "" {
		return ""
	}

	defCacheMu.RLock()
	if cached, ok := defCache[word]; ok && cached != "" {
		defCacheMu.RUnlock()
		return cached
	}
	defCacheMu.RUnlock()

	// 1. Try local SQLite Dictionary.db
	meaning := fetchFromSQLiteDictionary(word)
	if meaning == "" && strings.HasSuffix(word, "s") {
		meaning = fetchFromSQLiteDictionary(strings.TrimSuffix(word, "s"))
	}

	// 2. Fallback to Datamuse API
	if meaning == "" {
		meaning = fetchFromDatamuse(word)
		if meaning == "" && strings.HasSuffix(word, "s") {
			meaning = fetchFromDatamuse(strings.TrimSuffix(word, "s"))
		}
	}

	// 3. Fallback to Wiktionary REST API
	if meaning == "" {
		meaning = fetchFromWiktionary(word)
	}

	// 4. Fallback to builtin definitions
	if meaning == "" {
		meaning = getBuiltinDefinition(word)
	}

	if meaning != "" {
		defCacheMu.Lock()
		defCache[word] = meaning
		defCacheMu.Unlock()
	}

	return meaning
}

func fetchFromSQLiteDictionary(word string) string {
	db, err := getDictionaryDB()
	if err != nil || db == nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	query := `SELECT wordtype, definition FROM entries WHERE word = ? COLLATE NOCASE ORDER BY CASE WHEN wordtype LIKE 'n%' THEN 1 WHEN wordtype LIKE 'v%' THEN 2 WHEN wordtype LIKE 'a%' THEN 3 ELSE 4 END LIMIT 1`
	var wordtype, definition string
	err = db.QueryRowContext(ctx, query, word).Scan(&wordtype, &definition)
	if err != nil {
		return ""
	}

	pos := formatWordType(wordtype)
	return cleanAndMaskDefinition(definition, word, pos)
}

func formatWordType(wt string) string {
	wt = strings.TrimSpace(strings.ToLower(wt))
	if wt == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(wt, "n"):
		return "noun"
	case strings.HasPrefix(wt, "v"):
		return "verb"
	case strings.HasPrefix(wt, "a.") || strings.HasPrefix(wt, "adj"):
		return "adjective"
	case strings.HasPrefix(wt, "adv"):
		return "adverb"
	case strings.HasPrefix(wt, "prep"):
		return "preposition"
	case strings.HasPrefix(wt, "pron"):
		return "pronoun"
	case strings.HasPrefix(wt, "conj"):
		return "conjunction"
	case strings.HasPrefix(wt, "interj"):
		return "interjection"
	default:
		return strings.TrimSuffix(wt, ".")
	}
}

func fetchFromDatamuse(word string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	reqURL := "https://api.datamuse.com/words?sp=" + url.PathEscape(word) + "&md=d&max=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "WhatsRook/1.0 (Dictionary Client)")

	resp, err := GameHTTPClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()

	var results []struct {
		Word string   `json:"word"`
		Defs []string `json:"defs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil || len(results) == 0 || len(results[0].Defs) == 0 {
		return ""
	}

	for _, rawDef := range results[0].Defs {
		parts := strings.SplitN(rawDef, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		posCode := strings.ToLower(parts[0])
		defText := strings.TrimSpace(parts[1])

		// Skip songs, albums, movies/films, abbreviations, acronyms, transliterations, or non-definitions
		if strings.HasPrefix(defText, "\"") ||
			strings.HasPrefix(defText, "Acronym of") ||
			strings.HasPrefix(defText, "Abbreviation of") ||
			strings.Contains(defText, "translit") ||
			strings.Contains(defText, "Eurovision") ||
			strings.Contains(defText, "film directed by") ||
			strings.Contains(defText, "movie directed by") ||
			(strings.HasPrefix(defText, "the ") && strings.Contains(defText, "album")) {
			continue
		}

		pos := ""
		switch posCode {
		case "n":
			pos = "noun"
		case "v":
			pos = "verb"
		case "adj":
			pos = "adjective"
		case "adv":
			pos = "adverb"
		default:
			pos = posCode
		}

		cleaned := cleanAndMaskDefinition(defText, word, pos)
		if cleaned != "" {
			return cleaned
		}
	}

	return ""
}

func fetchFromWiktionary(word string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	reqURL := "https://en.wiktionary.org/api/rest_v1/page/definition/" + url.PathEscape(word)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "WhatsRook/1.0 (Dictionary Client)")

	resp, err := GameHTTPClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()

	var data map[string][]struct {
		PartOfSpeech string `json:"partOfSpeech"`
		Definitions  []struct {
			Definition string `json:"definition"`
		} `json:"definitions"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return ""
	}

	enEntries, ok := data["en"]
	if !ok || len(enEntries) == 0 {
		return ""
	}

	for _, entry := range enEntries {
		for _, d := range entry.Definitions {
			raw := stripHTMLTags(d.Definition)
			if raw != "" && !strings.HasPrefix(raw, "Alternative form of") && !strings.HasPrefix(raw, "Synonym of") {
				return cleanAndMaskDefinition(raw, word, strings.ToLower(entry.PartOfSpeech))
			}
		}
	}

	return ""
}

func stripHTMLTags(s string) string {
	re := regexp.MustCompile(`<[^>]*>`)
	cleaned := re.ReplaceAllString(s, "")
	cleaned = strings.ReplaceAll(cleaned, "&nbsp;", " ")
	cleaned = strings.ReplaceAll(cleaned, "&amp;", "&")
	cleaned = strings.ReplaceAll(cleaned, "&quot;", "\"")
	cleaned = strings.ReplaceAll(cleaned, "&#39;", "'")
	return strings.TrimSpace(cleaned)
}

func cleanAndMaskDefinition(def, word, pos string) string {
	def = strings.TrimSpace(def)
	if def == "" {
		return ""
	}

	// Collapse multiple whitespaces and newlines into single spaces
	spaceRe := regexp.MustCompile(`\s+`)
	def = spaceRe.ReplaceAllString(def, " ")

	// Remove trailing period
	def = strings.TrimSuffix(def, ".")

	// Mask occurrences of the word inside the definition so it doesn't give away the answer
	if word != "" {
		re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(word) + `s?\b`)
		def = re.ReplaceAllString(def, "____")
	}

	// Truncate overly long definitions
	if len(def) > 220 {
		idx := strings.LastIndex(def[:217], " ")
		if idx > 120 {
			def = def[:idx] + "..."
		} else {
			def = def[:217] + "..."
		}
	}

	// Capitalize first letter
	if len(def) > 0 {
		runes := []rune(def)
		runes[0] = unicode.ToUpper(runes[0])
		def = string(runes)
	}

	if !strings.HasSuffix(def, ".") && !strings.HasSuffix(def, "...") && !strings.HasSuffix(def, "!") {
		def += "."
	}

	if pos != "" {
		return whatsrook.Sprintf("[%s] %s", pos, def)
	}
	return def
}

// scrambleString returns a scrambled version of the string.
func scrambleString(s string) string {
	runes := []rune(s)
	// Try up to 10 times to get a different arrangement
	for range 10 {
		rand.Shuffle(len(runes), func(i, j int) {
			runes[i], runes[j] = runes[j], runes[i]
		})
		scrambled := string(runes)
		if scrambled != s {
			return scrambled
		}
	}
	// If all attempts produce same string (e.g., "aaa"), return as-is
	return string(runes)
}

// GetCXPTitle maps cumulative XP (CXP) to player titles.
func GetCXPTitle(xp int) string {
	switch {
	case xp >= 12000:
		return "Legendary Master"
	case xp >= 7000:
		return "Legend"
	case xp >= 3500:
		return "Prolific"
	case xp >= 1500:
		return "Master"
	case xp >= 500:
		return "Pro"
	case xp >= 100:
		return "Beginner"
	default:
		return "Novice"
	}
}

var builtinDefinitions = map[string]string{
	"cat":              "[noun] A small domesticated carnivorous mammal with soft fur, a short snout, and retractile claws.",
	"dog":              "[noun] A domesticated carnivorous mammal that typically has a long snout and an acute sense of smell.",
	"bat":              "[noun] A nocturnal flying mammal with forelimbs adapted for flight.",
	"rat":              "[noun] A rodent that resembles a mouse but is larger.",
	"hat":              "[noun] A shaped covering for the head worn for warmth, as a fashion item, or as part of a uniform.",
	"sun":              "[noun] The star round which the earth orbits.",
	"run":              "[verb] Move at a speed faster than a walk, never having both or all the feet on the ground at the same time.",
	"fun":              "[noun] Enjoyment, amusement, or lighthearted pleasure.",
	"pen":              "[noun] An instrument for writing or drawing with ink.",
	"cup":              "[noun] A small bowl-shaped container for drinking from, typically having a handle.",
	"box":              "[noun] A container with a flat base and sides, typically square or rectangular.",
	"fox":              "[noun] A carnivorous mammal of the dog family with a pointed muzzle and bushy tail.",
	"map":              "[noun] A diagrammatic representation of an area of land or sea showing physical features.",
	"tap":              "[verb] Strike something with a quick light blow or blows.",
	"nap":              "[noun] A short sleep, especially during the day.",
	"lip":              "[noun] Either of the two fleshy parts which form the upper and lower edges of the opening of the mouth.",
	"hip":              "[noun] A projection of the pelvis and upper thigh bone on each side of the body.",
	"rib":              "[noun] Each of a series of slender curved bones articulated in pairs to the spine.",
	"web":              "[noun] A network of fine threads constructed by a spider.",
	"mud":              "[noun] Soft, sticky matter consisting of a mixture of earth and water.",
	"sky":              "[noun] The region of the atmosphere and outer space seen from the earth.",
	"ice":              "[noun] Frozen water, a brittle, transparent crystalline solid.",
	"bed":              "[noun] A piece of furniture for sleep or rest.",
	"owl":              "[noun] A nocturnal bird of prey with large forward-facing eyes and a hawk-like beak.",
	"bus":              "[noun] A large motor vehicle carrying passengers by road.",
	"key":              "[noun] A small piece of shaped metal with incisions cut to fit the wards of a particular lock.",
	"ant":              "[noun] A small insect, often with a sting, that usually lives in a complex social colony.",
	"fly":              "[verb] Move through the air using wings.",
	"arm":              "[noun] Each of the two upper limbs of the human body from the shoulder to the hand.",
	"art":              "[noun] The expression or application of human creative skill and imagination.",
	"bird":             "[noun] A warm-blooded egg-laying vertebrate distinguished by the possession of feathers, wings, and a beak.",
	"fish":             "[noun] A limbless cold-blooded vertebrate animal with gills and fins living wholly in water.",
	"tree":             "[noun] A woody perennial plant, typically having a single stem or trunk growing to a considerable height.",
	"book":             "[noun] A written or printed work consisting of pages glued or sewn together along one side.",
	"desk":             "[noun] A piece of furniture with a flat or sloped surface and typically with drawers.",
	"lamp":             "[noun] A device for giving light, especially one consisting of an electric bulb.",
	"door":             "[noun] A hinged, sliding, or revolving barrier at the entrance to a building, room, or vehicle.",
	"road":             "[noun] A wide way leading from one place to another, especially one with a specially prepared surface.",
	"star":             "[noun] A fixed luminous point in the night sky that is a large, remote incandescent body.",
	"moon":             "[noun] The natural satellite of the earth, visible by reflected light from the sun.",
	"hand":             "[noun] The end part of a person's arm beyond the wrist.",
	"foot":             "[noun] The lower extremity of the leg below the ankle.",
	"head":             "[noun] The upper part of the human body, containing the brain, mouth, and sense organs.",
	"face":             "[noun] The front part of a person's head from the forehead to the chin.",
	"time":             "[noun] The indefinite continued progress of existence and events in the past, present, and future.",
	"love":             "[noun] An intense feeling of deep affection.",
	"hope":             "[noun] A feeling of expectation and desire for a certain thing to happen.",
	"fire":             "[noun] Combustion or burning, in which substances combine chemically with oxygen from the air.",
	"wind":             "[noun] The perceptible natural movement of the air, especially in the form of a current of air.",
	"rain":             "[noun] Moisture condensed from the atmosphere that falls visibly in separate drops.",
	"gold":             "[noun] A yellow precious metal, the chemical element of atomic number 79.",
	"ring":             "[noun] A small circular band, typically of precious metal, worn on a finger as an ornament.",
	"snow":             "[noun] Atmospheric water vapor frozen into ice crystals and falling in light white flakes.",
	"king":             "[noun] The male ruler of an independent state, especially one who inherits the position by right of birth.",
	"duck":             "[noun] A waterbird with a broad blunt bill, short legs, and webbed feet.",
	"bear":             "[noun] A large, heavy mammal that walks on the soles of its feet, having thick fur and a very short tail.",
	"lion":             "[noun] A large tawny-colored cat that lives in prides, native to Africa and northwestern India.",
	"frog":             "[noun] A tailless amphibian with a short squat body, moist smooth skin, and very long hind legs for leaping.",
	"leaf":             "[noun] A flattened structure of a higher plant, typically green and blade-like, that is attached to a stem.",
	"rose":             "[noun] A prickly bush or shrub that typically bears fragrant, beautiful flowers.",
	"apple":            "[noun] A common, round fruit produced by a tree, cultivated in temperate climates.",
	"grape":            "[noun] A green or purple berry growing in clusters on a grapevine, eaten as fruit or used in making wine.",
	"lemon":            "[noun] A yellow, oval citrus fruit with thick skin and fragrant, acidic juice.",
	"mango":            "[noun] A fleshy, oval, yellowish-red tropical fruit that is eaten ripe or used green for pickles or chutneys.",
	"peach":            "[noun] A round stone fruit with juicy yellow flesh and downy pinkish-yellow skin.",
	"chair":            "[noun] A separate seat for one person, typically with four legs and a back.",
	"table":            "[noun] A piece of furniture with a flat top and one or more legs, providing a level surface for eating or working.",
	"house":            "[noun] A building for human habitation, especially one that is lived in by a family or small group.",
	"water":            "[noun] A colorless, transparent, odorless liquid that forms the seas, lakes, rivers, and rain.",
	"earth":            "[noun] The planet on which we live; the world.",
	"music":            "[noun] Vocal or instrumental sounds combined in such a way as to produce beauty of form, harmony, and expression of emotion.",
	"dance":            "[verb] Move rhythmically to music, typically following a set sequence of steps.",
	"smile":            "[verb] Form one's features into a pleased, kind, or amused expression.",
	"laugh":            "[verb] Make the spontaneous sounds and movements of the face and body that are the instinctive expressions of lively amusement.",
	"dream":            "[noun] A series of thoughts, images, and sensations occurring in a person's mind during sleep.",
	"light":            "[noun] The natural agent that stimulates sight and makes things visible.",
	"night":            "[noun] The period of darkness in each twenty-four hours; the time from sunset to sunrise.",
	"world":            "[noun] The earth, together with all of its countries and peoples.",
	"peace":            "[noun] Freedom from disturbance; tranquility.",
	"power":            "[noun] The ability to do something or act in a particular way, especially as a faculty or quality.",
	"banana":           "[noun] A long curved fruit which grows in clusters and has soft pulpy flesh and yellow skin when ripe.",
	"orange":           "[noun] A round juicy citrus fruit with a tough bright reddish-yellow rind.",
	"cherry":           "[noun] A small, round stone fruit that is typically bright or dark red.",
	"rabbit":           "[noun] A burrowing, gregarious, plant-eating mammal with long ears, long hind legs, and a short tail.",
	"turtle":           "[noun] A slow-moving reptile, enclosed in a scaly or leathery domed shell into which it can retract its head and legs.",
	"window":           "[noun] An opening in a wall or door that is fitted with glass to let in light or air.",
	"garden":           "[noun] A piece of ground adjoining a house, in which grass, flowers, and vegetables may be grown.",
	"forest":           "[noun] A large area covered chiefly with trees and undergrowth.",
	"bridge":           "[noun] A structure carrying a road, path, railroad, or canal across a river, ravine, road, or railroad.",
	"castle":           "[noun] A large building, typically of the medieval period, fortified against attack with thick walls and towers.",
	"island":           "[noun] A piece of land surrounded by water.",
	"desert":           "[noun] A dry, barren area of land, especially one covered with sand, that is desolate and waterless.",
	"planet":           "[noun] A celestial body moving in an elliptical orbit around a star.",
	"rocket":           "[noun] A cylindrical projectile that can be propelled to a great height or distance by the combustion of fuel.",
	"puzzle":           "[noun] A game, toy, or problem designed to test ingenuity or knowledge.",
	"secret":           "[noun] Something that is kept or meant to be kept unknown or unseen by others.",
	"winter":           "[noun] The coldest season of the year, between autumn and spring.",
	"summer":           "[noun] The warmest season of the year, between spring and autumn.",
	"spring":           "[noun] The season after winter and before summer, in which vegetation begins to appear.",
	"autumn":           "[noun] The season after summer and before winter, in which fruits and crops are harvested and leaves fall.",
	"dolphin":          "[noun] A small gregarious toothed whale that typically has a beaklike snout and a curved dorsal fin.",
	"penguin":          "[noun] A flightless seabird of southern hemisphere with webbed feet and flippers for swimming.",
	"giraffe":          "[noun] A large African mammal with a very long neck and forelegs, having a coat patterned with brown patches.",
	"leopard":          "[noun] A large, solitary cat that has a fawn or brownish coat with black spots.",
	"dinosaur":         "[noun] A fossil reptile of the Mesozoic era, often reaching an enormous size.",
	"elephant":         "[noun] A very large herbivorous mammal with a trunk, large ears, and tusks.",
	"kangaroo":         "[noun] A large herbivorous marsupial with a long powerful tail and strongly developed hind legs for leaping.",
	"mountain":         "[noun] A large natural elevation of the earth's surface rising abruptly from the surrounding level.",
	"sapphire":         "[noun] A transparent precious stone, typically blue, that is a variety of corundum.",
	"crocodile":        "[noun] A large predatory semiaquatic reptile with long jaws, long tail, short legs, and a brawny body.",
	"alligator":        "[noun] A large semiaquatic reptile similar to a crocodile but with a broader head and darker skin.",
	"chameleon":        "[noun] A small slow-moving Old World lizard with a prehensile tail, extensible tongue, and eyes that move independently.",
	"butterfly":        "[noun] An insect with two pairs of large, typically brightly colored wings.",
	"waterfall":        "[noun] A cascade of water falling from a height, formed when a river or stream flows over a precipice.",
	"chimpanzee":       "[noun] An ape with dark hair and a lighter face, native to the forests of west and central Africa.",
	"orangutan":        "[noun] A large mainly solitary arboreal ape with long reddish hair and long arms.",
	"watermelon":       "[noun] The large fruit of a trailing plant, with a green rind and sweet, watery, usually red pulp.",
	"strawberry":       "[noun] A sweet soft red fruit with a seed-studded surface.",
	"helicopter":       "[noun] A type of aircraft that derives both lift and propulsion from horizontally revolving overhead rotors.",
	"locomotive":       "[noun] A powered rail vehicle used for pulling trains.",
	"caterpillar":      "[noun] The larval stage of a butterfly or moth, having a segmented worm-like body with three pairs of true legs.",
	"grasshopper":      "[noun] A plant-eating insect with long hind legs adapted for leaping and often a sound-producing organ on the hind legs.",
	"hippopotamus":     "[noun] A large thick-skinned semiaquatic African mammal with massive jaws and large tusks.",
	"trigonometry":     "[noun] The branch of mathematics dealing with the relations of the sides and angles of triangles and with the relevant functions.",
	"parallelogram":    "[noun] A four-sided plane rectilinear figure with opposite sides parallel.",
	"biotechnology":    "[noun] The exploitation of biological processes for industrial and other purposes.",
	"accomplishment":   "[noun] Something that has been achieved successfully.",
	"administration":   "[noun] The management of any office, business, or organization.",
	"congratulations":  "[noun] Words expressing praise for an achievement or good wishes on a special occasion.",
	"characteristics":  "[noun] Features or qualities belonging typically to a person, place, or thing and serving to identify them.",
	"characterization": "[noun] The creation or construction of a fictional character, or the description of features.",
	"incomprehensible": "[adjective] Not able to be understood; not intelligible.",
}

func getBuiltinDefinition(word string) string {
	if def, ok := builtinDefinitions[word]; ok && def != "" {
		return def
	}
	return ""
}
