package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"whatsrook/util/cache"
	"whatsrook/util/logger"

	"go.mau.fi/util/dbutil"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
)

const (
	filterCacheTTL          = 24 * time.Hour
	filterNegativeCacheTTL  = 10 * time.Minute
	settingCacheTTL         = 24 * time.Hour
	settingNegativeCacheTTL = 10 * time.Minute
	cacheSentinelNil        = "__nil__"
)

type CallMediaKind string

const (
	CallMediaAudio CallMediaKind = "audio"
	CallMediaVideo CallMediaKind = "video"
)

// Structural Data Types (replacing GORM structs)

type BotSetting struct {
	OurJID string
	Key    string
	Value  string
}

type CallMediaConfig struct {
	OurJID    string
	JID       string
	Kind      string
	FilePath  string
	UpdatedAt int64
}

type BotFilter struct {
	OurJID       string
	TriggerWord  string
	MessageProto string
}

type BotBGM struct {
	OurJID       string
	TriggerWord  string
	MessageProto string
}

type BotStickerCmd struct {
	OurJID        string
	StickerSHA256 string
	CommandName   string
}

type GroupStats struct {
	OurJID   string
	GroupJID string
	UserJID  string
	DateStr  string
	MsgCount int
}

type BotUserXP struct {
	OurJID    string
	UserJID   string
	XP        int64
	Level     int
	Messages  int64
	Stickers  int64
	Commands  int64
	UpdatedAt int64
	TTTWins   int
	TTTLosses int
	TTTDraws  int
	WCGWins   int
	WCGGames  int
	WCGRating int
}

type BotGroupUserXP struct {
	OurJID          string
	GroupJID        string
	UserJID         string
	XP              int64
	TTTWins         int
	TTTLosses       int
	TTTDraws        int
	WCGWins         int
	WCGGames        int
	WCGRating       int
	UnscrambleWins  int
	UnscrambleScore int
}

type CachedGroup struct {
	OurJID                 string
	JID                    string
	Name                   string
	Topic                  string
	TopicID                string
	TopicSetAt             time.Time
	TopicSetBy             string
	OwnerJID               string
	CreatedAt              time.Time
	IsLocked               bool
	IsAnnounce             bool
	IsEphemeral            bool
	EphemeralDuration      uint32
	MembershipApprovalMode bool
	IsIncognito            bool
	IsCommunity            bool
	ParentJID              string
	LinkedParentJID        string
	IsDefaultSubgroup      bool
	IsGeneralChat          bool
	ParticipantCount       int
	AdminCount             int
	UpdatedAt              time.Time
}

type CachedGroupParticipant struct {
	OurJID       string
	GroupJID     string
	UserJID      string
	LID          string
	IsAdmin      bool
	IsSuperAdmin bool
	DisplayName  string
}

type CachedNewsletter struct {
	OurJID           string
	JID              string
	Name             string
	Description      string
	InviteCode       string
	SubscribersCount int64
	Verification     string
	Role             string
	MuteState        string
	PictureURL       string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type GroupParticipantMetadata struct {
	JID          types.JID `json:"jid"`
	LID          types.JID `json:"lid"`
	IsAdmin      bool      `json:"is_admin"`
	IsSuperAdmin bool      `json:"is_super_admin"`
	DisplayName  string    `json:"display_name,omitempty"`
}

type GroupMetadata struct {
	JID                    types.JID                  `json:"jid"`
	Name                   string                     `json:"name"`
	Topic                  string                     `json:"topic"`
	TopicID                string                     `json:"topic_id,omitempty"`
	TopicSetAt             time.Time                  `json:"topic_set_at"`
	TopicSetBy             types.JID                  `json:"topic_set_by"`
	OwnerJID               types.JID                  `json:"owner_jid"`
	CreatedAt              time.Time                  `json:"created_at"`
	IsLocked               bool                       `json:"is_locked"`
	IsAnnounce             bool                       `json:"is_announce"`
	IsEphemeral            bool                       `json:"is_ephemeral"`
	EphemeralDuration      uint32                     `json:"ephemeral_duration"`
	MembershipApprovalMode bool                       `json:"membership_approval_mode"`
	IsIncognito            bool                       `json:"is_incognito"`
	IsCommunity            bool                       `json:"is_community"`
	ParentJID              types.JID                  `json:"parent_jid"`
	LinkedParentJID        types.JID                  `json:"linked_parent_jid"`
	IsDefaultSubgroup      bool                       `json:"is_default_subgroup"`
	IsGeneralChat          bool                       `json:"is_general_chat"`
	Participants           []GroupParticipantMetadata `json:"participants,omitempty"`
	ParticipantCount       int                        `json:"participant_count"`
	AdminCount             int                        `json:"admin_count"`
	UpdatedAt              time.Time                  `json:"updated_at"`
}

type NewsletterMetadata struct {
	JID              types.JID `json:"jid"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	InviteCode       string    `json:"invite_code,omitempty"`
	SubscribersCount int64     `json:"subscribers_count"`
	Verification     string    `json:"verification,omitempty"`
	Role             string    `json:"role,omitempty"`
	MuteState        string    `json:"mute_state,omitempty"`
	PictureURL       string    `json:"picture_url,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Utility Helpers

func ourJIDStr(s *sqlstore.SQLStore) string {
	if s == nil {
		return ""
	}
	if s.JID != "" {
		if parsed, err := types.ParseJID(s.JID); err == nil && !parsed.IsEmpty() {
			return parsed.ToNonAD().String()
		}
		return s.JID
	}
	return ""
}

func filterCacheKey(ourJID, trigger string) string {
	return "filter:" + ourJID + ":" + trigger
}

func bgmCacheKey(ourJID, trigger string) string {
	return "bgm:" + ourJID + ":" + trigger
}

func stickerCacheKey(ourJID, shaHex string) string {
	return "stkcmd:" + ourJID + ":" + shaHex
}

func settingCacheKey(ourJID, key string) string {
	return "setting:" + ourJID + ":" + key
}

func getDBFromStore(s *sqlstore.SQLStore) (*dbutil.Database, error) {
	if s == nil {
		return nil, fmt.Errorf("store is nil")
	}
	db := s.GetDB()
	if db == nil {
		return nil, fmt.Errorf("database handle is nil")
	}
	return db, nil
}

// Schema Migration Framework

type Migration struct {
	Version     int
	Description string
	Up          func(ctx context.Context, db *dbutil.Database) error
}

const (
	createSchemaVersionTableQuery = `
		CREATE TABLE IF NOT EXISTS cli_schema_version (
			version INTEGER PRIMARY KEY,
			applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			description TEXT NOT NULL DEFAULT ''
		);
	`
	getAppliedVersionsQuery = `SELECT version FROM cli_schema_version ORDER BY version ASC`
	recordVersionQuery      = `INSERT INTO cli_schema_version (version, applied_at, description) VALUES ($1, $2, $3)`
)

func getMigrations() []Migration {
	return []Migration{
		{Version: 1, Description: "Initialize base custom bot tables with primary keys and constraints", Up: migration1InitialSchema},
		{Version: 2, Description: "Repair PostgreSQL constraints, column types, and unique indexes", Up: migration2RepairConstraintsAndColumns},
		{Version: 3, Description: "Add performance and indexing optimization for stats, leaderboard, and stickers", Up: migration3PerformanceIndexes},
		{Version: 4, Description: "Ensure unique index on call_media_config(jid, kind) for PostgreSQL upserts", Up: migration4CallMediaUniqueIndex},
		{Version: 5, Description: "Repair call_media_config updated_at default and drop not-null constraint", Up: migration5RepairCallMediaDefaults},
		{Version: 6, Description: "Add cached groups, communities, participants, and newsletters tables", Up: migration6CachedGroupsAndChannels},
		{Version: 7, Description: "Scope all custom bot tables by our_jid for full per-session isolation in shared databases", Up: migration7SessionIsolation},
	}
}

func RunMigrations(ctx context.Context, db *dbutil.Database) error {
	if db == nil {
		return fmt.Errorf("database handle is nil")
	}

	if _, err := db.Exec(ctx, createSchemaVersionTableQuery); err != nil {
		return fmt.Errorf("failed to create schema version table: %w", err)
	}

	rows, err := db.Query(ctx, getAppliedVersionsQuery)
	if err != nil {
		return fmt.Errorf("failed to query applied schema versions: %w", err)
	}
	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err == nil {
			applied[v] = true
		}
	}
	rows.Close()

	for _, m := range getMigrations() {
		if applied[m.Version] {
			continue
		}

		logger.Info("Applying CLI database migration...", "version", m.Version, "description", m.Description, "dialect", db.Dialect.String())
		if err := m.Up(ctx, db); err != nil {
			return fmt.Errorf("migration v%d (%s) failed: %w", m.Version, m.Description, err)
		}

		if _, err := db.Exec(ctx, recordVersionQuery, m.Version, time.Now().UTC(), m.Description); err != nil {
			return fmt.Errorf("failed to record migration v%d: %w", m.Version, err)
		}
		logger.Info("Successfully applied CLI database migration", "version", m.Version)
	}

	return nil
}

func migration1InitialSchema(ctx context.Context, db *dbutil.Database) error {
	schemas := []string{
		`CREATE TABLE IF NOT EXISTS bot_settings (
			our_jid TEXT NOT NULL DEFAULT '',
			key     TEXT NOT NULL,
			value   TEXT NOT NULL,
			PRIMARY KEY (our_jid, key)
		)`,
		`CREATE TABLE IF NOT EXISTS call_media_config (
			our_jid    TEXT NOT NULL DEFAULT '',
			jid        TEXT NOT NULL,
			kind       TEXT NOT NULL DEFAULT 'audio',
			file_path  TEXT NOT NULL,
			updated_at BIGINT DEFAULT 0,
			PRIMARY KEY (our_jid, jid, kind)
		)`,
		`CREATE INDEX IF NOT EXISTS call_media_config_our_jid_idx ON call_media_config (our_jid)`,
		`CREATE TABLE IF NOT EXISTS bot_filters (
			our_jid       TEXT NOT NULL DEFAULT '',
			trigger_word  TEXT NOT NULL,
			message_proto TEXT NOT NULL,
			PRIMARY KEY (our_jid, trigger_word)
		)`,
		`CREATE TABLE IF NOT EXISTS bot_bgm (
			our_jid       TEXT NOT NULL DEFAULT '',
			trigger_word  TEXT NOT NULL,
			message_proto TEXT NOT NULL,
			PRIMARY KEY (our_jid, trigger_word)
		)`,
		`CREATE TABLE IF NOT EXISTS group_stats (
			our_jid   TEXT NOT NULL DEFAULT '',
			group_jid TEXT NOT NULL,
			user_jid  TEXT NOT NULL,
			date_str  TEXT NOT NULL,
			msg_count INTEGER NOT NULL DEFAULT 1,
			PRIMARY KEY (our_jid, group_jid, user_jid, date_str)
		)`,
		`CREATE TABLE IF NOT EXISTS bot_sticker_cmds (
			our_jid        TEXT NOT NULL DEFAULT '',
			sticker_sha256 TEXT NOT NULL,
			command_name   TEXT NOT NULL,
			PRIMARY KEY (our_jid, sticker_sha256)
		)`,
		`CREATE TABLE IF NOT EXISTS bot_user_xp (
			our_jid    TEXT NOT NULL DEFAULT '',
			user_jid   TEXT NOT NULL,
			xp         INTEGER NOT NULL DEFAULT 0,
			level      INTEGER NOT NULL DEFAULT 1,
			messages   INTEGER NOT NULL DEFAULT 0,
			stickers   INTEGER NOT NULL DEFAULT 0,
			commands   INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0,
			ttt_wins   INTEGER NOT NULL DEFAULT 0,
			ttt_losses INTEGER NOT NULL DEFAULT 0,
			ttt_draws  INTEGER NOT NULL DEFAULT 0,
			wcg_wins   INTEGER NOT NULL DEFAULT 0,
			wcg_games  INTEGER NOT NULL DEFAULT 0,
			wcg_rating INTEGER NOT NULL DEFAULT 1000,
			PRIMARY KEY (our_jid, user_jid)
		)`,
		`CREATE TABLE IF NOT EXISTS bot_group_user_xp (
			our_jid          TEXT NOT NULL DEFAULT '',
			group_jid        TEXT NOT NULL,
			user_jid         TEXT NOT NULL,
			xp               INTEGER NOT NULL DEFAULT 0,
			ttt_wins         INTEGER NOT NULL DEFAULT 0,
			ttt_losses       INTEGER NOT NULL DEFAULT 0,
			ttt_draws        INTEGER NOT NULL DEFAULT 0,
			wcg_wins         INTEGER NOT NULL DEFAULT 0,
			wcg_games        INTEGER NOT NULL DEFAULT 0,
			wcg_rating       INTEGER NOT NULL DEFAULT 1000,
			unscramble_wins  INTEGER NOT NULL DEFAULT 0,
			unscramble_score INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (our_jid, group_jid, user_jid)
		)`,
	}
	for _, query := range schemas {
		if _, err := db.Exec(ctx, query); err != nil {
			return fmt.Errorf("failed executing schema %q: %w", query, err)
		}
	}
	return nil
}

func migration2RepairConstraintsAndColumns(ctx context.Context, db *dbutil.Database) error {
	_ = EnsureCustomColumnExists(ctx, db, "bot_settings", "our_jid", "TEXT DEFAULT ''")
	if db.Dialect == dbutil.Postgres {
		_, _ = db.Exec(ctx, "ALTER TABLE bot_settings ALTER COLUMN our_jid SET DEFAULT ''")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_settings ALTER COLUMN our_jid DROP NOT NULL")
		_, _ = db.Exec(ctx, "ALTER TABLE call_media_config ALTER COLUMN our_jid SET DEFAULT ''")
		_, _ = db.Exec(ctx, "ALTER TABLE call_media_config ALTER COLUMN our_jid DROP NOT NULL")
		_, _ = db.Exec(ctx, "ALTER TABLE call_media_config ALTER COLUMN updated_at SET DEFAULT 0")
		_, _ = db.Exec(ctx, "ALTER TABLE call_media_config ALTER COLUMN updated_at DROP NOT NULL")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_filters ALTER COLUMN our_jid SET DEFAULT ''")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_filters ALTER COLUMN our_jid DROP NOT NULL")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_bgm ALTER COLUMN our_jid SET DEFAULT ''")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_bgm ALTER COLUMN our_jid DROP NOT NULL")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_sticker_cmds ALTER COLUMN our_jid SET DEFAULT ''")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_sticker_cmds ALTER COLUMN our_jid DROP NOT NULL")
	}
	_, _ = db.Exec(ctx, "UPDATE bot_settings SET our_jid = '' WHERE our_jid IS NULL")

	hasSender, _ := TableHasColumn(ctx, db, "call_media_config", "sender")
	hasJID, _ := TableHasColumn(ctx, db, "call_media_config", "jid")
	if hasSender && !hasJID {
		if _, err := db.Exec(ctx, "ALTER TABLE call_media_config RENAME COLUMN sender TO jid"); err != nil {
			_ = EnsureCustomColumnExists(ctx, db, "call_media_config", "jid", "TEXT DEFAULT ''")
			_, _ = db.Exec(ctx, "UPDATE call_media_config SET jid = sender WHERE jid = '' OR jid IS NULL")
		}
	}
	_ = EnsureCustomColumnExists(ctx, db, "call_media_config", "our_jid", "TEXT DEFAULT ''")
	_ = EnsureCustomColumnExists(ctx, db, "call_media_config", "updated_at", "BIGINT DEFAULT 0")

	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "wcg_wins", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "wcg_games", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "wcg_rating", "INTEGER DEFAULT 1000")

	_ = EnsureCustomColumnExists(ctx, db, "bot_group_user_xp", "wcg_wins", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_group_user_xp", "wcg_games", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_group_user_xp", "wcg_rating", "INTEGER DEFAULT 1000")
	_ = EnsureCustomColumnExists(ctx, db, "bot_group_user_xp", "unscramble_wins", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_group_user_xp", "unscramble_score", "INTEGER DEFAULT 0")

	_ = EnsureCustomColumnExists(ctx, db, "whatsmeow_contacts", "username", "TEXT")
	return nil
}

func migration3PerformanceIndexes(ctx context.Context, db *dbutil.Database) error {
	indexes := []string{
		"CREATE INDEX IF NOT EXISTS group_stats_date_idx ON group_stats (date_str)",
		"CREATE INDEX IF NOT EXISTS group_stats_user_idx ON group_stats (group_jid, user_jid)",
		"CREATE INDEX IF NOT EXISTS bot_group_user_xp_leaderboard_idx ON bot_group_user_xp (group_jid, xp DESC)",
		"CREATE INDEX IF NOT EXISTS bot_sticker_cmds_name_idx ON bot_sticker_cmds (our_jid, command_name)",
		"CREATE INDEX IF NOT EXISTS call_media_config_our_jid_idx ON call_media_config (our_jid)",
	}
	for _, idxQuery := range indexes {
		_, _ = db.Exec(ctx, idxQuery)
	}
	return nil
}

func migration4CallMediaUniqueIndex(ctx context.Context, db *dbutil.Database) error {
	return nil
}

func migration5RepairCallMediaDefaults(ctx context.Context, db *dbutil.Database) error {
	if db.Dialect == dbutil.Postgres {
		_, _ = db.Exec(ctx, "ALTER TABLE call_media_config ALTER COLUMN updated_at SET DEFAULT 0")
		_, _ = db.Exec(ctx, "ALTER TABLE call_media_config ALTER COLUMN updated_at DROP NOT NULL")
	}
	_, _ = db.Exec(ctx, "UPDATE call_media_config SET updated_at = 0 WHERE updated_at IS NULL")
	return nil
}

func migration6CachedGroupsAndChannels(ctx context.Context, db *dbutil.Database) error {
	schemas := []string{
		`CREATE TABLE IF NOT EXISTS cached_groups (
			our_jid                   TEXT NOT NULL DEFAULT '',
			jid                       TEXT NOT NULL,
			name                      TEXT NOT NULL DEFAULT '',
			topic                     TEXT NOT NULL DEFAULT '',
			topic_id                  TEXT DEFAULT '',
			topic_set_at              TIMESTAMP,
			topic_set_by              TEXT DEFAULT '',
			owner_jid                 TEXT NOT NULL DEFAULT '',
			created_at                TIMESTAMP,
			is_locked                 BOOLEAN DEFAULT FALSE,
			is_announce               BOOLEAN DEFAULT FALSE,
			is_ephemeral              BOOLEAN DEFAULT FALSE,
			ephemeral_duration        INTEGER DEFAULT 0,
			membership_approval_mode  BOOLEAN DEFAULT FALSE,
			is_incognito              BOOLEAN DEFAULT FALSE,
			is_community              BOOLEAN DEFAULT FALSE,
			parent_jid                TEXT DEFAULT '',
			linked_parent_jid         TEXT DEFAULT '',
			is_default_subgroup       BOOLEAN DEFAULT FALSE,
			is_general_chat           BOOLEAN DEFAULT FALSE,
			participant_count         INTEGER DEFAULT 0,
			admin_count               INTEGER DEFAULT 0,
			updated_at                TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (our_jid, jid)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS cached_groups_our_jid_jid_idx ON cached_groups (our_jid, jid)`,
		`CREATE INDEX IF NOT EXISTS cached_groups_parent_idx ON cached_groups (our_jid, parent_jid)`,
		`CREATE INDEX IF NOT EXISTS cached_groups_community_idx ON cached_groups (our_jid, is_community)`,
		`CREATE TABLE IF NOT EXISTS cached_group_participants (
			our_jid        TEXT NOT NULL DEFAULT '',
			group_jid      TEXT NOT NULL,
			user_jid       TEXT NOT NULL,
			lid            TEXT DEFAULT '',
			is_admin       BOOLEAN DEFAULT FALSE,
			is_super_admin BOOLEAN DEFAULT FALSE,
			display_name   TEXT DEFAULT '',
			PRIMARY KEY (our_jid, group_jid, user_jid)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS cached_group_participants_pk_idx ON cached_group_participants (our_jid, group_jid, user_jid)`,
		`CREATE INDEX IF NOT EXISTS cached_participants_user_idx ON cached_group_participants (our_jid, user_jid)`,
		`CREATE TABLE IF NOT EXISTS cached_newsletters (
			our_jid           TEXT NOT NULL DEFAULT '',
			jid               TEXT NOT NULL,
			name              TEXT NOT NULL DEFAULT '',
			description       TEXT NOT NULL DEFAULT '',
			invite_code       TEXT DEFAULT '',
			subscribers_count BIGINT DEFAULT 0,
			verification      TEXT DEFAULT '',
			role              TEXT DEFAULT '',
			mute_state        TEXT DEFAULT '',
			picture_url       TEXT DEFAULT '',
			created_at        TIMESTAMP,
			updated_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (our_jid, jid)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS cached_newsletters_our_jid_jid_idx ON cached_newsletters (our_jid, jid)`,
	}

	for _, s := range schemas {
		if _, err := db.Exec(ctx, s); err != nil {
			return fmt.Errorf("failed executing schema %q: %w", s, err)
		}
	}
	return nil
}

func migration7SessionIsolation(ctx context.Context, db *dbutil.Database) error {
	_, _ = db.Exec(ctx, "DROP INDEX IF EXISTS bot_settings_key_idx")
	_ = EnsureCustomColumnExists(ctx, db, "bot_settings", "our_jid", "TEXT NOT NULL DEFAULT ''")
	_, _ = db.Exec(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS bot_settings_our_jid_key_idx ON bot_settings (our_jid, key)")

	_, _ = db.Exec(ctx, "DROP INDEX IF EXISTS call_media_config_jid_kind_idx")
	_ = EnsureCustomColumnExists(ctx, db, "call_media_config", "our_jid", "TEXT NOT NULL DEFAULT ''")
	_, _ = db.Exec(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS call_media_config_our_jid_jid_kind_idx ON call_media_config (our_jid, jid, kind)")

	_ = EnsureCustomColumnExists(ctx, db, "group_stats", "our_jid", "TEXT NOT NULL DEFAULT ''")
	_, _ = db.Exec(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS group_stats_our_jid_group_user_date_idx ON group_stats (our_jid, group_jid, user_jid, date_str)")

	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "our_jid", "TEXT NOT NULL DEFAULT ''")
	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "level", "INTEGER DEFAULT 1")
	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "messages", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "stickers", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "commands", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "updated_at", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "ttt_wins", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "ttt_losses", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "ttt_draws", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "wcg_wins", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "wcg_games", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_user_xp", "wcg_rating", "INTEGER DEFAULT 1000")
	_, _ = db.Exec(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS bot_user_xp_our_jid_user_idx ON bot_user_xp (our_jid, user_jid)")

	_ = EnsureCustomColumnExists(ctx, db, "bot_group_user_xp", "our_jid", "TEXT NOT NULL DEFAULT ''")
	_ = EnsureCustomColumnExists(ctx, db, "bot_group_user_xp", "ttt_wins", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_group_user_xp", "ttt_losses", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_group_user_xp", "ttt_draws", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_group_user_xp", "wcg_wins", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_group_user_xp", "wcg_games", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_group_user_xp", "wcg_rating", "INTEGER DEFAULT 1000")
	_ = EnsureCustomColumnExists(ctx, db, "bot_group_user_xp", "unscramble_wins", "INTEGER DEFAULT 0")
	_ = EnsureCustomColumnExists(ctx, db, "bot_group_user_xp", "unscramble_score", "INTEGER DEFAULT 0")
	_, _ = db.Exec(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS bot_group_user_xp_our_jid_group_user_idx ON bot_group_user_xp (our_jid, group_jid, user_jid)")

	_ = EnsureCustomColumnExists(ctx, db, "cached_groups", "is_general_chat", "BOOLEAN DEFAULT FALSE")

	if db.Dialect == dbutil.Postgres {
		_, _ = db.Exec(ctx, "ALTER TABLE cached_groups ADD COLUMN IF NOT EXISTS is_general_chat BOOLEAN DEFAULT FALSE")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_settings ADD COLUMN IF NOT EXISTS our_jid TEXT NOT NULL DEFAULT ''")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_settings DROP CONSTRAINT IF EXISTS bot_settings_pkey")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_settings DROP CONSTRAINT IF EXISTS bot_settings_key_key")
		_, _ = db.Exec(ctx, "DROP INDEX IF EXISTS bot_settings_key_idx")
		_, _ = db.Exec(ctx, "DELETE FROM bot_settings a USING bot_settings b WHERE a.ctid < b.ctid AND a.our_jid = b.our_jid AND a.key = b.key")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_settings ADD PRIMARY KEY (our_jid, key)")
		_, _ = db.Exec(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS bot_settings_our_jid_key_idx ON bot_settings (our_jid, key)")

		_, _ = db.Exec(ctx, "ALTER TABLE call_media_config ADD COLUMN IF NOT EXISTS our_jid TEXT NOT NULL DEFAULT ''")
		_, _ = db.Exec(ctx, "ALTER TABLE call_media_config DROP CONSTRAINT IF EXISTS call_media_config_pkey")
		_, _ = db.Exec(ctx, "ALTER TABLE call_media_config DROP CONSTRAINT IF EXISTS call_media_config_jid_kind_key")
		_, _ = db.Exec(ctx, "DROP INDEX IF EXISTS call_media_config_jid_kind_idx")
		_, _ = db.Exec(ctx, "DELETE FROM call_media_config a USING call_media_config b WHERE a.ctid < b.ctid AND a.our_jid = b.our_jid AND a.jid = b.jid AND a.kind = b.kind")
		_, _ = db.Exec(ctx, "ALTER TABLE call_media_config ADD PRIMARY KEY (our_jid, jid, kind)")
		_, _ = db.Exec(ctx, "CREATE UNIQUE INDEX IF NOT EXISTS call_media_config_our_jid_jid_kind_idx ON call_media_config (our_jid, jid, kind)")

		_, _ = db.Exec(ctx, "ALTER TABLE group_stats ADD COLUMN IF NOT EXISTS our_jid TEXT NOT NULL DEFAULT ''")
		_, _ = db.Exec(ctx, "ALTER TABLE group_stats DROP CONSTRAINT IF EXISTS group_stats_pkey")
		_, _ = db.Exec(ctx, "DELETE FROM group_stats a USING group_stats b WHERE a.ctid < b.ctid AND a.our_jid = b.our_jid AND a.group_jid = b.group_jid AND a.user_jid = b.user_jid AND a.date_str = b.date_str")
		_, _ = db.Exec(ctx, "ALTER TABLE group_stats ADD PRIMARY KEY (our_jid, group_jid, user_jid, date_str)")

		_, _ = db.Exec(ctx, "ALTER TABLE bot_user_xp ADD COLUMN IF NOT EXISTS our_jid TEXT NOT NULL DEFAULT ''")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_user_xp ADD COLUMN IF NOT EXISTS level INTEGER NOT NULL DEFAULT 1")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_user_xp ADD COLUMN IF NOT EXISTS messages BIGINT NOT NULL DEFAULT 0")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_user_xp ADD COLUMN IF NOT EXISTS stickers BIGINT NOT NULL DEFAULT 0")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_user_xp ADD COLUMN IF NOT EXISTS commands BIGINT NOT NULL DEFAULT 0")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_user_xp ADD COLUMN IF NOT EXISTS updated_at BIGINT NOT NULL DEFAULT 0")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_user_xp DROP CONSTRAINT IF EXISTS bot_user_xp_pkey")
		_, _ = db.Exec(ctx, "DELETE FROM bot_user_xp a USING bot_user_xp b WHERE a.ctid < b.ctid AND a.our_jid = b.our_jid AND a.user_jid = b.user_jid")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_user_xp ADD PRIMARY KEY (our_jid, user_jid)")

		_, _ = db.Exec(ctx, "ALTER TABLE bot_group_user_xp ADD COLUMN IF NOT EXISTS our_jid TEXT NOT NULL DEFAULT ''")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_group_user_xp ADD COLUMN IF NOT EXISTS unscramble_wins INTEGER NOT NULL DEFAULT 0")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_group_user_xp ADD COLUMN IF NOT EXISTS unscramble_score INTEGER NOT NULL DEFAULT 0")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_group_user_xp DROP CONSTRAINT IF EXISTS bot_group_user_xp_pkey")
		_, _ = db.Exec(ctx, "DELETE FROM bot_group_user_xp a USING bot_group_user_xp b WHERE a.ctid < b.ctid AND a.our_jid = b.our_jid AND a.group_jid = b.group_jid AND a.user_jid = b.user_jid")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_group_user_xp ADD PRIMARY KEY (our_jid, group_jid, user_jid)")

		_, _ = db.Exec(ctx, "ALTER TABLE bot_filters ADD COLUMN IF NOT EXISTS our_jid TEXT NOT NULL DEFAULT ''")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_filters DROP CONSTRAINT IF EXISTS bot_filters_pkey")
		_, _ = db.Exec(ctx, "DELETE FROM bot_filters a USING bot_filters b WHERE a.ctid < b.ctid AND a.our_jid = b.our_jid AND a.trigger_word = b.trigger_word")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_filters ADD PRIMARY KEY (our_jid, trigger_word)")

		_, _ = db.Exec(ctx, "ALTER TABLE bot_bgm ADD COLUMN IF NOT EXISTS our_jid TEXT NOT NULL DEFAULT ''")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_bgm DROP CONSTRAINT IF EXISTS bot_bgm_pkey")
		_, _ = db.Exec(ctx, "DELETE FROM bot_bgm a USING bot_bgm b WHERE a.ctid < b.ctid AND a.our_jid = b.our_jid AND a.trigger_word = b.trigger_word")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_bgm ADD PRIMARY KEY (our_jid, trigger_word)")

		_, _ = db.Exec(ctx, "ALTER TABLE bot_sticker_cmds ADD COLUMN IF NOT EXISTS our_jid TEXT NOT NULL DEFAULT ''")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_sticker_cmds DROP CONSTRAINT IF EXISTS bot_sticker_cmds_pkey")
		_, _ = db.Exec(ctx, "DELETE FROM bot_sticker_cmds a USING bot_sticker_cmds b WHERE a.ctid < b.ctid AND a.our_jid = b.our_jid AND a.sticker_sha256 = b.sticker_sha256")
		_, _ = db.Exec(ctx, "ALTER TABLE bot_sticker_cmds ADD PRIMARY KEY (our_jid, sticker_sha256)")
	}

	return nil
}

// Database Diagnostics & Initialization Functions

var tablesInitOnce sync.Once

func TableHasColumn(ctx context.Context, db *dbutil.Database, table, column string) (bool, error) {
	if db == nil {
		return false, fmt.Errorf("nil database")
	}

	var exists bool
	err := db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns 
			WHERE table_name = $1 AND column_name = $2
		)
	`, strings.ToLower(table), strings.ToLower(column)).Scan(&exists)
	return exists, err
}

func EnsureCustomColumnExists(ctx context.Context, db *dbutil.Database, table, column, colDef string) error {
	if db == nil {
		return fmt.Errorf("nil database")
	}

	hasCol, err := TableHasColumn(ctx, db, table, column)
	if err == nil && hasCol {
		if db.Dialect == dbutil.Postgres {
			upperDef := strings.ToUpper(colDef)
			if strings.Contains(upperDef, "DEFAULT") {
				parts := strings.SplitN(upperDef, "DEFAULT", 2)
				if len(parts) == 2 {
					defaultVal := strings.TrimSpace(colDef[len(parts[0])+7:])
					_, _ = db.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s", table, column, defaultVal))
				}
			}
			if !strings.Contains(upperDef, "NOT NULL") {
				_, _ = db.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL", table, column))
			}
		}
		return nil
	}

	alterCmd := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, colDef)
	_, err = db.Exec(ctx, alterCmd)
	if err != nil {
		errStr := strings.ToLower(err.Error())
		if strings.Contains(errStr, "duplicate column") ||
			strings.Contains(errStr, "already exists") ||
			strings.Contains(errStr, "42701") {
			return nil
		}
		return err
	}
	return nil
}

func InitTables(ctx context.Context, s *sqlstore.SQLStore) {
	if s == nil {
		return
	}
	tablesInitOnce.Do(func() {
		db := s.GetDB()
		if db == nil {
			return
		}

		if err := RunMigrations(ctx, db); err != nil {
			logger.Error("InitTables: failed to execute schema migrations", "err", err, "dialect", db.Dialect.String())
		}
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := PrewarmSettings(bgCtx, s); err != nil {
				logger.Warn("Failed to prewarm bot settings", "err", err)
			}
		}()
	})
}

// Low-Level Direct SQL Operations (Bot Data Store)

// Filters & BGM

func GetFilter(ctx context.Context, s *sqlstore.SQLStore, trigger string) (string, error) {
	if s == nil {
		return "", nil
	}
	start := time.Now()
	db, err := getDBFromStore(s)
	if err != nil {
		return "", err
	}
	ourJID := ourJIDStr(s)
	cacheKey := filterCacheKey(ourJID, trigger)

	if val, ok, _ := cache.Get(ctx, cacheKey); ok {
		if val == cacheSentinelNil {
			return "", nil
		}
		return val, nil
	}

	dbStart := time.Now()
	var msgProto string
	query := `
		SELECT message_proto FROM bot_filters 
		WHERE (our_jid = $1 OR our_jid = $2 OR our_jid = '' OR our_jid IS NULL) AND trigger_word = $3
		ORDER BY CASE WHEN our_jid = $1 THEN 1 WHEN our_jid = $2 THEN 2 ELSE 3 END 
		LIMIT 1
	`
	err = db.QueryRow(ctx, query, ourJID, s.JID, trigger).Scan(&msgProto)
	durDB := time.Since(dbStart)
	if durDB > 1*time.Millisecond {
		logger.Debug("[PERF] store.GetFilter DB query", "trigger", trigger, "dbDuration", durDB, "total", time.Since(start))
	}
	if errors.Is(err, sql.ErrNoRows) {
		_ = cache.Set(ctx, cacheKey, cacheSentinelNil, filterNegativeCacheTTL)
		return "", nil
	}
	if err != nil {
		return "", err
	}

	_ = cache.Set(ctx, cacheKey, msgProto, filterCacheTTL)
	if s.JID != "" && s.JID != ourJID {
		_ = cache.Set(ctx, filterCacheKey(s.JID, trigger), msgProto, filterCacheTTL)
	}
	return msgProto, nil
}

func PutFilter(ctx context.Context, s *sqlstore.SQLStore, trigger, messageProto string) error {
	if s == nil {
		return nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return err
	}
	ourJID := ourJIDStr(s)

	cacheKey := filterCacheKey(ourJID, trigger)
	_ = cache.Set(ctx, cacheKey, messageProto, filterCacheTTL)
	if s.JID != "" && s.JID != ourJID {
		_ = cache.Set(ctx, filterCacheKey(s.JID, trigger), messageProto, filterCacheTTL)
	}

	query := `
		INSERT INTO bot_filters (our_jid, trigger_word, message_proto)
		VALUES ($1, $2, $3)
		ON CONFLICT (our_jid, trigger_word) 
		DO UPDATE SET message_proto = EXCLUDED.message_proto
	`
	_, err = db.Exec(ctx, query, ourJID, trigger, messageProto)
	return err
}

func DeleteFilter(ctx context.Context, s *sqlstore.SQLStore, trigger string) error {
	if s == nil {
		return nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return err
	}
	ourJID := ourJIDStr(s)

	_ = cache.Delete(ctx, filterCacheKey(ourJID, trigger))
	if s.JID != "" && s.JID != ourJID {
		_ = cache.Delete(ctx, filterCacheKey(s.JID, trigger))
	}
	_ = cache.Delete(ctx, filterCacheKey("", trigger))

	query := `
		DELETE FROM bot_filters 
		WHERE (our_jid = $1 OR our_jid = $2 OR our_jid = '' OR our_jid IS NULL) AND trigger_word = $3
	`
	_, err = db.Exec(ctx, query, ourJID, s.JID, trigger)
	return err
}

func ListFilters(ctx context.Context, s *sqlstore.SQLStore) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return nil, err
	}
	ourJID := ourJIDStr(s)

	query := `
		SELECT trigger_word FROM bot_filters 
		WHERE our_jid = $1 OR our_jid = $2 OR our_jid = '' OR our_jid IS NULL 
		ORDER BY trigger_word ASC
	`
	rows, err := db.Query(ctx, query, ourJID, s.JID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err == nil {
			triggers = append(triggers, t)
		}
	}
	return triggers, nil
}

func GetBGM(ctx context.Context, s *sqlstore.SQLStore, trigger string) (string, error) {
	if s == nil {
		return "", nil
	}
	start := time.Now()
	db, err := getDBFromStore(s)
	if err != nil {
		return "", err
	}
	ourJID := ourJIDStr(s)
	cacheKey := bgmCacheKey(ourJID, trigger)

	if val, ok, _ := cache.Get(ctx, cacheKey); ok {
		if val == cacheSentinelNil {
			return "", nil
		}
		return val, nil
	}

	dbStart := time.Now()
	var msgProto string
	query := `
		SELECT message_proto FROM bot_bgm 
		WHERE (our_jid = $1 OR our_jid = $2 OR our_jid = '' OR our_jid IS NULL) AND trigger_word = $3
		ORDER BY CASE WHEN our_jid = $1 THEN 1 WHEN our_jid = $2 THEN 2 ELSE 3 END 
		LIMIT 1
	`
	err = db.QueryRow(ctx, query, ourJID, s.JID, trigger).Scan(&msgProto)
	durDB := time.Since(dbStart)
	if durDB > 1*time.Millisecond {
		logger.Debug("[PERF] store.GetBGM DB query", "trigger", trigger, "dbDuration", durDB, "total", time.Since(start))
	}
	if errors.Is(err, sql.ErrNoRows) {
		_ = cache.Set(ctx, cacheKey, cacheSentinelNil, filterNegativeCacheTTL)
		return "", nil
	}
	if err != nil {
		return "", err
	}

	_ = cache.Set(ctx, cacheKey, msgProto, filterCacheTTL)
	if s.JID != "" && s.JID != ourJID {
		_ = cache.Set(ctx, bgmCacheKey(s.JID, trigger), msgProto, filterCacheTTL)
	}
	return msgProto, nil
}

func PutBGM(ctx context.Context, s *sqlstore.SQLStore, trigger, messageProto string) error {
	if s == nil {
		return nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return err
	}
	ourJID := ourJIDStr(s)

	cacheKey := bgmCacheKey(ourJID, trigger)
	_ = cache.Set(ctx, cacheKey, messageProto, filterCacheTTL)
	if s.JID != "" && s.JID != ourJID {
		_ = cache.Set(ctx, bgmCacheKey(s.JID, trigger), messageProto, filterCacheTTL)
	}

	query := `
		INSERT INTO bot_bgm (our_jid, trigger_word, message_proto)
		VALUES ($1, $2, $3)
		ON CONFLICT (our_jid, trigger_word) 
		DO UPDATE SET message_proto = EXCLUDED.message_proto
	`
	_, err = db.Exec(ctx, query, ourJID, trigger, messageProto)
	return err
}

func DeleteBGM(ctx context.Context, s *sqlstore.SQLStore, trigger string) error {
	if s == nil {
		return nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return err
	}
	ourJID := ourJIDStr(s)

	_ = cache.Delete(ctx, bgmCacheKey(ourJID, trigger))
	if s.JID != "" && s.JID != ourJID {
		_ = cache.Delete(ctx, bgmCacheKey(s.JID, trigger))
	}
	_ = cache.Delete(ctx, bgmCacheKey("", trigger))

	query := `
		DELETE FROM bot_bgm 
		WHERE (our_jid = $1 OR our_jid = $2 OR our_jid = '' OR our_jid IS NULL) AND trigger_word = $3
	`
	_, err = db.Exec(ctx, query, ourJID, s.JID, trigger)
	return err
}

func ListBGMs(ctx context.Context, s *sqlstore.SQLStore) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return nil, err
	}
	ourJID := ourJIDStr(s)

	query := `
		SELECT trigger_word FROM bot_bgm 
		WHERE our_jid = $1 OR our_jid = $2 OR our_jid = '' OR our_jid IS NULL 
		ORDER BY trigger_word ASC
	`
	rows, err := db.Query(ctx, query, ourJID, s.JID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err == nil {
			triggers = append(triggers, t)
		}
	}
	return triggers, nil
}

// Sticker Commands

func GetStickerCmd(ctx context.Context, s *sqlstore.SQLStore, shaHex string) (string, error) {
	if s == nil {
		return "", nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return "", err
	}
	ourJID := ourJIDStr(s)
	cacheKey := stickerCacheKey(ourJID, shaHex)

	if val, ok, _ := cache.Get(ctx, cacheKey); ok {
		if val == cacheSentinelNil {
			return "", nil
		}
		return val, nil
	}

	var cmdName string
	query := `SELECT command_name FROM bot_sticker_cmds WHERE our_jid = $1 AND sticker_sha256 = $2 LIMIT 1`
	err = db.QueryRow(ctx, query, ourJID, shaHex).Scan(&cmdName)
	if errors.Is(err, sql.ErrNoRows) {
		_ = cache.Set(ctx, cacheKey, cacheSentinelNil, filterNegativeCacheTTL)
		return "", nil
	}
	if err != nil {
		return "", err
	}

	_ = cache.Set(ctx, cacheKey, cmdName, filterCacheTTL)
	return cmdName, nil
}

func PutStickerCmd(ctx context.Context, s *sqlstore.SQLStore, shaHex, cmdName string) error {
	if s == nil {
		return nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return err
	}
	ourJID := ourJIDStr(s)

	cacheKey := stickerCacheKey(ourJID, shaHex)
	_ = cache.Set(ctx, cacheKey, cmdName, filterCacheTTL)

	query := `
		INSERT INTO bot_sticker_cmds (our_jid, sticker_sha256, command_name)
		VALUES ($1, $2, $3)
		ON CONFLICT (our_jid, sticker_sha256)
		DO UPDATE SET command_name = EXCLUDED.command_name
	`
	_, err = db.Exec(ctx, query, ourJID, shaHex, cmdName)
	return err
}

func DeleteStickerCmdBySHA(ctx context.Context, s *sqlstore.SQLStore, shaHex string) error {
	if s == nil {
		return nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return err
	}
	ourJID := ourJIDStr(s)

	_ = cache.Delete(ctx, stickerCacheKey(ourJID, shaHex))

	query := `DELETE FROM bot_sticker_cmds WHERE our_jid = $1 AND sticker_sha256 = $2`
	_, err = db.Exec(ctx, query, ourJID, shaHex)
	return err
}

func DeleteStickerCmdByName(ctx context.Context, s *sqlstore.SQLStore, cmdName string) error {
	if s == nil {
		return nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return err
	}
	ourJID := ourJIDStr(s)

	_ = cache.DeletePrefix(ctx, "stkcmd:"+ourJID+":")

	query := `DELETE FROM bot_sticker_cmds WHERE our_jid = $1 AND (command_name = $2 OR command_name LIKE $3)`
	_, err = db.Exec(ctx, query, ourJID, cmdName, cmdName+" %")
	return err
}

func ListStickerCmds(ctx context.Context, s *sqlstore.SQLStore) ([]BotStickerCmd, error) {
	if s == nil {
		return nil, nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return nil, err
	}
	ourJID := ourJIDStr(s)

	query := `SELECT our_jid, sticker_sha256, command_name FROM bot_sticker_cmds WHERE our_jid = $1`
	rows, err := db.Query(ctx, query, ourJID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []BotStickerCmd
	for rows.Next() {
		var sc BotStickerCmd
		if err := rows.Scan(&sc.OurJID, &sc.StickerSHA256, &sc.CommandName); err == nil {
			list = append(list, sc)
		}
	}
	return list, nil
}

// Bot Settings

var (
	hotSettingsMu     sync.RWMutex
	hotSettingsCache  = make(map[string]string)
	hotSettingsLoaded atomic.Bool
)

// PrewarmSettings loads all bot_settings rows into hot in-memory cache at startup.
func PrewarmSettings(ctx context.Context, s *sqlstore.SQLStore) error {
	if s == nil {
		return nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return err
	}
	ourJID := ourJIDStr(s)
	query := `SELECT our_jid, key, value FROM bot_settings WHERE (our_jid = $1 OR our_jid = $2 OR our_jid = '' OR our_jid IS NULL)`
	rows, err := db.Query(ctx, query, ourJID, s.JID)
	if err != nil {
		return err
	}
	defer rows.Close()

	hotSettingsMu.Lock()
	count := 0
	for rows.Next() {
		var oJID, key, value string
		if err := rows.Scan(&oJID, &key, &value); err == nil {
			hotSettingsCache[settingCacheKey(ourJID, key)] = value
			_ = cache.Set(ctx, settingCacheKey(ourJID, key), value, settingCacheTTL)
			if s.JID != "" && s.JID != ourJID {
				hotSettingsCache[settingCacheKey(s.JID, key)] = value
				_ = cache.Set(ctx, settingCacheKey(s.JID, key), value, settingCacheTTL)
			}
			count++
		}
	}
	hotSettingsLoaded.Store(true)
	hotSettingsMu.Unlock()
	logger.Debug("[PERF] Prewarmed bot settings into memory", "count", count)
	return rows.Err()
}

func GetSetting(ctx context.Context, s *sqlstore.SQLStore, key string) (string, error) {
	if s == nil {
		return "", nil
	}
	ourJID := ourJIDStr(s)
	cacheKey := settingCacheKey(ourJID, key)

	hotSettingsMu.RLock()
	val, ok := hotSettingsCache[cacheKey]
	hotSettingsMu.RUnlock()
	if ok {
		return val, nil
	}

	if val, ok, _ := cache.Get(ctx, cacheKey); ok {
		hotSettingsMu.Lock()
		hotSettingsCache[cacheKey] = val
		hotSettingsMu.Unlock()
		return val, nil
	}

	if hotSettingsLoaded.Load() {
		return "", nil
	}

	start := time.Now()
	db, err := getDBFromStore(s)
	if err != nil {
		return "", err
	}

	dbStart := time.Now()
	var value string
	query := `
		SELECT value FROM bot_settings 
		WHERE (our_jid = $1 OR our_jid = $2 OR our_jid = '' OR our_jid IS NULL) AND key = $3
		ORDER BY CASE WHEN our_jid = $1 THEN 1 WHEN our_jid = $2 THEN 2 ELSE 3 END 
		LIMIT 1
	`
	err = db.QueryRow(ctx, query, ourJID, s.JID, key).Scan(&value)
	durDB := time.Since(dbStart)
	if durDB > 1*time.Millisecond {
		logger.Debug("[PERF] store.GetSetting DB query", "key", key, "dbDuration", durDB, "total", time.Since(start))
	}
	if errors.Is(err, sql.ErrNoRows) {
		hotSettingsMu.Lock()
		hotSettingsCache[cacheKey] = ""
		hotSettingsMu.Unlock()
		_ = cache.Set(ctx, cacheKey, "", settingNegativeCacheTTL)
		return "", nil
	}
	if err != nil {
		return "", err
	}

	hotSettingsMu.Lock()
	hotSettingsCache[cacheKey] = value
	if s.JID != "" && s.JID != ourJID {
		hotSettingsCache[settingCacheKey(s.JID, key)] = value
	}
	hotSettingsMu.Unlock()

	_ = cache.Set(ctx, cacheKey, value, settingCacheTTL)
	if s.JID != "" && s.JID != ourJID {
		_ = cache.Set(ctx, settingCacheKey(s.JID, key), value, settingCacheTTL)
	}
	return value, nil
}

func PutSetting(ctx context.Context, s *sqlstore.SQLStore, key, value string) error {
	if s == nil {
		return nil
	}
	ourJID := ourJIDStr(s)
	cacheKey := settingCacheKey(ourJID, key)

	hotSettingsMu.Lock()
	hotSettingsCache[cacheKey] = value
	if s.JID != "" && s.JID != ourJID {
		hotSettingsCache[settingCacheKey(s.JID, key)] = value
	}
	hotSettingsMu.Unlock()

	_ = cache.Set(ctx, cacheKey, value, settingCacheTTL)
	if s.JID != "" && s.JID != ourJID {
		_ = cache.Set(ctx, settingCacheKey(s.JID, key), value, settingCacheTTL)
	}

	db, err := getDBFromStore(s)
	if err != nil {
		return err
	}

	query := `
		INSERT INTO bot_settings (our_jid, key, value)
		VALUES ($1, $2, $3)
		ON CONFLICT (our_jid, key) 
		DO UPDATE SET value = EXCLUDED.value
	`
	_, err = db.Exec(ctx, query, ourJID, key, value)
	return err
}

func DeleteSetting(ctx context.Context, s *sqlstore.SQLStore, key string) error {
	if s == nil {
		return nil
	}
	ourJID := ourJIDStr(s)
	cacheKey := settingCacheKey(ourJID, key)

	hotSettingsMu.Lock()
	delete(hotSettingsCache, cacheKey)
	if s.JID != "" && s.JID != ourJID {
		delete(hotSettingsCache, settingCacheKey(s.JID, key))
	}
	delete(hotSettingsCache, settingCacheKey("", key))
	hotSettingsMu.Unlock()

	_ = cache.Delete(ctx, cacheKey)
	if s.JID != "" && s.JID != ourJID {
		_ = cache.Delete(ctx, settingCacheKey(s.JID, key))
	}
	_ = cache.Delete(ctx, settingCacheKey("", key))

	db, err := getDBFromStore(s)
	if err != nil {
		return err
	}

	query := `
		DELETE FROM bot_settings 
		WHERE (our_jid = $1 OR our_jid = $2 OR our_jid = '' OR our_jid IS NULL) AND key = $3
	`
	_, err = db.Exec(ctx, query, ourJID, s.JID, key)
	return err
}

func ListSettingsWithPrefixes(ctx context.Context, s *sqlstore.SQLStore, prefixes ...string) ([]BotSetting, error) {
	if s == nil {
		return nil, nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return nil, err
	}
	ourJID := ourJIDStr(s)

	baseQuery := `SELECT our_jid, key, value FROM bot_settings WHERE (our_jid = $1 OR our_jid = $2 OR our_jid = '' OR our_jid IS NULL)`
	var args []any
	args = append(args, ourJID, s.JID)

	if len(prefixes) > 0 {
		var conds []string
		for i, p := range prefixes {
			conds = append(conds, fmt.Sprintf("key LIKE $%d", i+3))
			args = append(args, p+"%")
		}
		baseQuery += " AND (" + strings.Join(conds, " OR ") + ")"
	}

	rows, err := db.Query(ctx, baseQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []BotSetting
	for rows.Next() {
		var bs BotSetting
		if err := rows.Scan(&bs.OurJID, &bs.Key, &bs.Value); err == nil {
			results = append(results, bs)
		}
	}
	return results, nil
}

// Call Media Config

func GetCallMediaConfig(ctx context.Context, s *sqlstore.SQLStore, jid types.JID, kind CallMediaKind) (string, error) {
	if s == nil {
		return "", nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return "", err
	}

	ourJID := ourJIDStr(s)
	jidStr := jid.ToNonAD().String()

	var filePath string
	query := `
		SELECT file_path FROM call_media_config 
		WHERE (our_jid = $1 OR our_jid = $2 OR our_jid = '' OR our_jid IS NULL) AND jid = $3 AND kind = $4
		ORDER BY CASE WHEN our_jid = $1 THEN 1 WHEN our_jid = $2 THEN 2 ELSE 3 END 
		LIMIT 1
	`
	err = db.QueryRow(ctx, query, ourJID, s.JID, jidStr, string(kind)).Scan(&filePath)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return filePath, nil
}

func PutCallMediaConfig(ctx context.Context, s *sqlstore.SQLStore, jid types.JID, kind CallMediaKind, filePath string) error {
	if s == nil {
		return nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return err
	}

	ourJID := ourJIDStr(s)
	jidStr := jid.ToNonAD().String()
	updatedAt := time.Now().Unix()

	query := `
		INSERT INTO call_media_config (our_jid, jid, kind, file_path, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (our_jid, jid, kind)
		DO UPDATE SET file_path = EXCLUDED.file_path, updated_at = EXCLUDED.updated_at
	`
	_, err = db.Exec(ctx, query, ourJID, jidStr, string(kind), filePath, updatedAt)
	return err
}

// Stats & Game XP Tracking

func StoreGroupMessage(ctx context.Context, s *sqlstore.SQLStore, chat, sender types.JID) {
	if s == nil {
		return
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return
	}

	ourJID := ourJIDStr(s)
	groupJID := chat.String()
	userJID := sender.ToNonAD().String()
	dateStr := time.Now().Format("2006-01-02")

	query := `
		INSERT INTO group_stats (our_jid, group_jid, user_jid, date_str, msg_count)
		VALUES ($1, $2, $3, $4, 1)
		ON CONFLICT (our_jid, group_jid, user_jid, date_str)
		DO UPDATE SET msg_count = COALESCE(group_stats.msg_count, 0) + 1
	`
	_, _ = db.Exec(ctx, query, ourJID, groupJID, userJID, dateStr)
}

func AddGroupUserTTTXP(ctx context.Context, s *sqlstore.SQLStore, groupJID, userJID string, amount, winInc, lossInc, drawInc int) error {
	if s == nil {
		return nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return err
	}
	ourJID := ourJIDStr(s)

	query := `
		INSERT INTO bot_group_user_xp (our_jid, group_jid, user_jid, xp, ttt_wins, ttt_losses, ttt_draws, wcg_rating)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1000)
		ON CONFLICT (our_jid, group_jid, user_jid)
		DO UPDATE SET 
			xp = CASE WHEN COALESCE(bot_group_user_xp.xp, 0) + EXCLUDED.xp < 0 THEN 0 ELSE COALESCE(bot_group_user_xp.xp, 0) + EXCLUDED.xp END,
			ttt_wins = COALESCE(bot_group_user_xp.ttt_wins, 0) + EXCLUDED.ttt_wins,
			ttt_losses = COALESCE(bot_group_user_xp.ttt_losses, 0) + EXCLUDED.ttt_losses,
			ttt_draws = COALESCE(bot_group_user_xp.ttt_draws, 0) + EXCLUDED.ttt_draws
	`
	_, err = db.Exec(ctx, query, ourJID, groupJID, userJID, amount, winInc, lossInc, drawInc)
	return err
}

func AddGroupUserWCGXP(ctx context.Context, s *sqlstore.SQLStore, groupJID, userJID string, amount, winInc, gameInc, ratingDelta int) error {
	if s == nil {
		return nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return err
	}
	ourJID := ourJIDStr(s)

	initRating := max(1000+ratingDelta, 100)

	query := `
		INSERT INTO bot_group_user_xp (our_jid, group_jid, user_jid, xp, wcg_wins, wcg_games, wcg_rating)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (our_jid, group_jid, user_jid)
		DO UPDATE SET 
			xp = CASE WHEN COALESCE(bot_group_user_xp.xp, 0) + EXCLUDED.xp < 0 THEN 0 ELSE COALESCE(bot_group_user_xp.xp, 0) + EXCLUDED.xp END,
			wcg_wins = COALESCE(bot_group_user_xp.wcg_wins, 0) + EXCLUDED.wcg_wins,
			wcg_games = COALESCE(bot_group_user_xp.wcg_games, 0) + EXCLUDED.wcg_games,
			wcg_rating = CASE WHEN COALESCE(bot_group_user_xp.wcg_rating, 1000) + $8 < 100 THEN 100 ELSE COALESCE(bot_group_user_xp.wcg_rating, 1000) + $8 END
	`
	_, err = db.Exec(ctx, query, ourJID, groupJID, userJID, amount, winInc, gameInc, initRating, ratingDelta)
	return err
}

func AddGroupUserUnscrambleXP(ctx context.Context, s *sqlstore.SQLStore, groupJID, userJID string, amount, winInc, scoreInc int) error {
	if s == nil {
		return nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return err
	}
	ourJID := ourJIDStr(s)

	query := `
		INSERT INTO bot_group_user_xp (our_jid, group_jid, user_jid, xp, unscramble_wins, unscramble_score, wcg_rating)
		VALUES ($1, $2, $3, $4, $5, $6, 1000)
		ON CONFLICT (our_jid, group_jid, user_jid)
		DO UPDATE SET 
			xp = CASE WHEN COALESCE(bot_group_user_xp.xp, 0) + EXCLUDED.xp < 0 THEN 0 ELSE COALESCE(bot_group_user_xp.xp, 0) + EXCLUDED.xp END,
			unscramble_wins = COALESCE(bot_group_user_xp.unscramble_wins, 0) + EXCLUDED.unscramble_wins,
			unscramble_score = COALESCE(bot_group_user_xp.unscramble_score, 0) + EXCLUDED.unscramble_score
	`
	_, err = db.Exec(ctx, query, ourJID, groupJID, userJID, amount, winInc, scoreInc)
	return err
}

func GetGroupLeaderboard(ctx context.Context, s *sqlstore.SQLStore, groupJID string) ([]BotGroupUserXP, error) {
	if s == nil {
		return nil, nil
	}
	db, err := getDBFromStore(s)
	if err != nil {
		return nil, err
	}
	ourJID := ourJIDStr(s)

	query := `
		SELECT our_jid, group_jid, user_jid, xp, ttt_wins, ttt_losses, ttt_draws, wcg_wins, wcg_games, wcg_rating, unscramble_wins, unscramble_score 
		FROM bot_group_user_xp 
		WHERE our_jid = $1 AND group_jid = $2 
		ORDER BY xp DESC, ttt_wins DESC
	`
	rows, err := db.Query(ctx, query, ourJID, groupJID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []BotGroupUserXP
	for rows.Next() {
		var entry BotGroupUserXP
		if err := rows.Scan(
			&entry.OurJID, &entry.GroupJID, &entry.UserJID, &entry.XP,
			&entry.TTTWins, &entry.TTTLosses, &entry.TTTDraws,
			&entry.WCGWins, &entry.WCGGames, &entry.WCGRating,
			&entry.UnscrambleWins, &entry.UnscrambleScore,
		); err == nil {
			list = append(list, entry)
		}
	}
	return list, nil
}

// Caching Operations (Groups & Newsletters)

func SaveCachedGroup(ctx context.Context, db *dbutil.Database, ourJID string, g *GroupMetadata) error {
	if db == nil || g == nil {
		return nil
	}

	now := time.Now().UTC()
	g.UpdatedAt = now

	query := `
		INSERT INTO cached_groups (
			our_jid, jid, name, topic, topic_id, topic_set_at, topic_set_by, owner_jid,
			created_at, is_locked, is_announce, is_ephemeral, ephemeral_duration,
			membership_approval_mode, is_incognito, is_community, parent_jid,
			linked_parent_jid, is_default_subgroup, is_general_chat, participant_count,
			admin_count, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8,
			$9, $10, $11, $12, $13,
			$14, $15, $16, $17,
			$18, $19, $20, $21,
			$22, $23
		)
		ON CONFLICT (our_jid, jid) DO UPDATE SET
			name = EXCLUDED.name,
			topic = EXCLUDED.topic,
			topic_id = EXCLUDED.topic_id,
			topic_set_at = EXCLUDED.topic_set_at,
			topic_set_by = EXCLUDED.topic_set_by,
			owner_jid = EXCLUDED.owner_jid,
			created_at = EXCLUDED.created_at,
			is_locked = EXCLUDED.is_locked,
			is_announce = EXCLUDED.is_announce,
			is_ephemeral = EXCLUDED.is_ephemeral,
			ephemeral_duration = EXCLUDED.ephemeral_duration,
			membership_approval_mode = EXCLUDED.membership_approval_mode,
			is_incognito = EXCLUDED.is_incognito,
			is_community = EXCLUDED.is_community,
			parent_jid = EXCLUDED.parent_jid,
			linked_parent_jid = EXCLUDED.linked_parent_jid,
			is_default_subgroup = EXCLUDED.is_default_subgroup,
			is_general_chat = EXCLUDED.is_general_chat,
			participant_count = EXCLUDED.participant_count,
			admin_count = EXCLUDED.admin_count,
			updated_at = EXCLUDED.updated_at
	`

	_, err := db.Exec(ctx, query,
		ourJID, g.JID.String(), g.Name, g.Topic, g.TopicID, g.TopicSetAt, g.TopicSetBy.String(), g.OwnerJID.String(),
		g.CreatedAt, g.IsLocked, g.IsAnnounce, g.IsEphemeral, g.EphemeralDuration,
		g.MembershipApprovalMode, g.IsIncognito, g.IsCommunity, g.ParentJID.String(),
		g.LinkedParentJID.String(), g.IsDefaultSubgroup, g.IsGeneralChat, g.ParticipantCount,
		g.AdminCount, now,
	)
	if err != nil {
		return fmt.Errorf("SaveCachedGroup failed for %s: %w", g.JID.String(), err)
	}

	if len(g.Participants) > 0 {
		_ = SaveCachedGroupParticipants(ctx, db, ourJID, g.JID.String(), g.Participants)
	}
	return nil
}

func SaveCachedGroupParticipants(ctx context.Context, db *dbutil.Database, ourJID, groupJID string, participants []GroupParticipantMetadata) error {
	if db == nil {
		return nil
	}

	_, _ = db.Exec(ctx, `DELETE FROM cached_group_participants WHERE our_jid = $1 AND group_jid = $2`, ourJID, groupJID)

	if len(participants) == 0 {
		return nil
	}

	seenUsers := make(map[string]bool)
	type participantRow struct {
		userJID      string
		lid          string
		isAdmin      bool
		isSuperAdmin bool
		displayName  string
	}
	validRows := make([]participantRow, 0, len(participants))
	for _, p := range participants {
		uStr := p.JID.String()
		if uStr == "" || seenUsers[uStr] {
			continue
		}
		seenUsers[uStr] = true
		validRows = append(validRows, participantRow{
			userJID:      uStr,
			lid:          p.LID.String(),
			isAdmin:      p.IsAdmin,
			isSuperAdmin: p.IsSuperAdmin,
			displayName:  p.DisplayName,
		})
	}

	const chunkSize = 50
	for i := 0; i < len(validRows); i += chunkSize {
		end := min(i+chunkSize, len(validRows))
		chunk := validRows[i:end]

		var queryBuilder strings.Builder
		queryBuilder.WriteString(`
			INSERT INTO cached_group_participants (our_jid, group_jid, user_jid, lid, is_admin, is_super_admin, display_name)
			VALUES `)

		args := make([]any, 0, len(chunk)*7)
		paramIndex := 1
		for j, row := range chunk {
			if j > 0 {
				queryBuilder.WriteString(",")
			}
			queryBuilder.WriteString(fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, $%d, $%d)",
				paramIndex, paramIndex+1, paramIndex+2, paramIndex+3, paramIndex+4, paramIndex+5, paramIndex+6))
			args = append(args, ourJID, groupJID, row.userJID, row.lid, row.isAdmin, row.isSuperAdmin, row.displayName)
			paramIndex += 7
		}
		queryBuilder.WriteString(`
			ON CONFLICT (our_jid, group_jid, user_jid) DO UPDATE SET
				lid = EXCLUDED.lid,
				is_admin = EXCLUDED.is_admin,
				is_super_admin = EXCLUDED.is_super_admin,
				display_name = EXCLUDED.display_name
		`)

		_, _ = db.Exec(ctx, queryBuilder.String(), args...)
	}
	return nil
}

func SaveCachedNewsletter(ctx context.Context, db *dbutil.Database, ourJID string, n *NewsletterMetadata) error {
	if db == nil || n == nil {
		return nil
	}

	now := time.Now().UTC()
	n.UpdatedAt = now

	query := `
		INSERT INTO cached_newsletters (
			our_jid, jid, name, description, invite_code, subscribers_count,
			verification, role, mute_state, picture_url, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (our_jid, jid) DO UPDATE SET
			name = EXCLUDED.name,
			description = EXCLUDED.description,
			invite_code = EXCLUDED.invite_code,
			subscribers_count = EXCLUDED.subscribers_count,
			verification = EXCLUDED.verification,
			role = EXCLUDED.role,
			mute_state = EXCLUDED.mute_state,
			picture_url = EXCLUDED.picture_url,
			created_at = EXCLUDED.created_at,
			updated_at = EXCLUDED.updated_at
	`

	_, err := db.Exec(ctx, query,
		ourJID, n.JID.String(), n.Name, n.Description, n.InviteCode, n.SubscribersCount,
		n.Verification, n.Role, n.MuteState, n.PictureURL, n.CreatedAt, now,
	)
	return err
}

func DeleteCachedGroup(ctx context.Context, db *dbutil.Database, ourJID, groupJID string) error {
	if db == nil {
		return nil
	}
	_, _ = db.Exec(ctx, `DELETE FROM cached_group_participants WHERE our_jid = $1 AND group_jid = $2`, ourJID, groupJID)
	_, err := db.Exec(ctx, `DELETE FROM cached_groups WHERE our_jid = $1 AND jid = $2`, ourJID, groupJID)
	return err
}

func DeleteCachedNewsletter(ctx context.Context, db *dbutil.Database, ourJID, newsletterJID string) error {
	if db == nil {
		return nil
	}
	_, err := db.Exec(ctx, `DELETE FROM cached_newsletters WHERE our_jid = $1 AND jid = $2`, ourJID, newsletterJID)
	return err
}

func LoadAllCachedGroups(ctx context.Context, db *dbutil.Database, ourJID string) ([]*GroupMetadata, error) {
	if db == nil {
		return nil, nil
	}

	normJID := ourJID
	if parsed, err := types.ParseJID(ourJID); err == nil && !parsed.IsEmpty() {
		normJID = parsed.ToNonAD().String()
	}

	partQuery := `
		SELECT group_jid, user_jid, lid, is_admin, is_super_admin, display_name 
		FROM cached_group_participants 
		WHERE our_jid = $1 OR our_jid = $2 OR our_jid = '' OR our_jid IS NULL
	`
	partRows, err := db.Query(ctx, partQuery, normJID, ourJID)
	if err != nil {
		return nil, err
	}
	defer partRows.Close()

	partMap := make(map[string][]GroupParticipantMetadata)
	for partRows.Next() {
		var groupJID, userJID, lid, displayName string
		var isAdmin, isSuperAdmin bool
		if err := partRows.Scan(&groupJID, &userJID, &lid, &isAdmin, &isSuperAdmin, &displayName); err == nil {
			uJID, _ := types.ParseJID(userJID)
			lJID, _ := types.ParseJID(lid)
			partMap[groupJID] = append(partMap[groupJID], GroupParticipantMetadata{
				JID:          uJID,
				LID:          lJID,
				IsAdmin:      isAdmin,
				IsSuperAdmin: isSuperAdmin,
				DisplayName:  displayName,
			})
		}
	}

	groupQuery := `
		SELECT 
			jid, name, topic, topic_id, topic_set_at, topic_set_by, owner_jid, created_at,
			is_locked, is_announce, is_ephemeral, ephemeral_duration, membership_approval_mode,
			is_incognito, is_community, parent_jid, linked_parent_jid, is_default_subgroup,
			is_general_chat, participant_count, admin_count, updated_at
		FROM cached_groups
		WHERE our_jid = $1 OR our_jid = $2 OR our_jid = '' OR our_jid IS NULL
		ORDER BY name ASC
	`
	rows, err := db.Query(ctx, groupQuery, normJID, ourJID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []*GroupMetadata
	for rows.Next() {
		var cg CachedGroup
		var topicSetBy, ownerJID, parentJID, linkedParentJID string

		err := rows.Scan(
			&cg.JID, &cg.Name, &cg.Topic, &cg.TopicID, &cg.TopicSetAt, &topicSetBy, &ownerJID, &cg.CreatedAt,
			&cg.IsLocked, &cg.IsAnnounce, &cg.IsEphemeral, &cg.EphemeralDuration, &cg.MembershipApprovalMode,
			&cg.IsIncognito, &cg.IsCommunity, &parentJID, &linkedParentJID, &cg.IsDefaultSubgroup,
			&cg.IsGeneralChat, &cg.ParticipantCount, &cg.AdminCount, &cg.UpdatedAt,
		)
		if err != nil {
			continue
		}

		gJID, _ := types.ParseJID(cg.JID)
		topicBy, _ := types.ParseJID(topicSetBy)
		oJID, _ := types.ParseJID(ownerJID)
		pJID, _ := types.ParseJID(parentJID)
		lpJID, _ := types.ParseJID(linkedParentJID)

		parts := partMap[cg.JID]
		pCount := cg.ParticipantCount
		if len(parts) > 0 {
			pCount = len(parts)
		}

		groups = append(groups, &GroupMetadata{
			JID:                    gJID,
			Name:                   cg.Name,
			Topic:                  cg.Topic,
			TopicID:                cg.TopicID,
			TopicSetAt:             cg.TopicSetAt,
			TopicSetBy:             topicBy,
			OwnerJID:               oJID,
			CreatedAt:              cg.CreatedAt,
			IsLocked:               cg.IsLocked,
			IsAnnounce:             cg.IsAnnounce,
			IsEphemeral:            cg.IsEphemeral,
			EphemeralDuration:      cg.EphemeralDuration,
			MembershipApprovalMode: cg.MembershipApprovalMode,
			IsIncognito:            cg.IsIncognito,
			IsCommunity:            cg.IsCommunity,
			ParentJID:              pJID,
			LinkedParentJID:        lpJID,
			IsDefaultSubgroup:      cg.IsDefaultSubgroup,
			IsGeneralChat:          cg.IsGeneralChat,
			Participants:           parts,
			ParticipantCount:       pCount,
			AdminCount:             cg.AdminCount,
			UpdatedAt:              cg.UpdatedAt,
		})
	}

	return groups, nil
}

func LoadAllCachedNewsletters(ctx context.Context, db *dbutil.Database, ourJID string) ([]*NewsletterMetadata, error) {
	if db == nil {
		return nil, nil
	}

	normJID := ourJID
	if parsed, err := types.ParseJID(ourJID); err == nil && !parsed.IsEmpty() {
		normJID = parsed.ToNonAD().String()
	}

	query := `
		SELECT jid, name, description, invite_code, subscribers_count, verification, role, mute_state, picture_url, created_at, updated_at
		FROM cached_newsletters
		WHERE our_jid = $1 OR our_jid = $2 OR our_jid = '' OR our_jid IS NULL
		ORDER BY name ASC
	`
	rows, err := db.Query(ctx, query, normJID, ourJID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var newsletters []*NewsletterMetadata
	for rows.Next() {
		var cn CachedNewsletter
		err := rows.Scan(
			&cn.JID, &cn.Name, &cn.Description, &cn.InviteCode, &cn.SubscribersCount,
			&cn.Verification, &cn.Role, &cn.MuteState, &cn.PictureURL, &cn.CreatedAt, &cn.UpdatedAt,
		)
		if err != nil {
			continue
		}

		nJID, _ := types.ParseJID(cn.JID)
		newsletters = append(newsletters, &NewsletterMetadata{
			JID:              nJID,
			Name:             cn.Name,
			Description:      cn.Description,
			InviteCode:       cn.InviteCode,
			SubscribersCount: cn.SubscribersCount,
			Verification:     cn.Verification,
			Role:             cn.Role,
			MuteState:        cn.MuteState,
			PictureURL:       cn.PictureURL,
			CreatedAt:        cn.CreatedAt,
			UpdatedAt:        cn.UpdatedAt,
		})
	}

	return newsletters, nil
}
