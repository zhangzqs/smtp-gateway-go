package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"gorm.io/gorm"

	"github.com/zhangzqs/smtp-gateway-go/internal/config"
	"github.com/zhangzqs/smtp-gateway-go/internal/infra/database"
)

//go:embed migrations/*/*.sql
var migrations embed.FS

var (
	ErrQueueFull = errors.New("queue capacity exceeded")
	ErrNotFound  = errors.New("message not found")
	ErrConflict  = errors.New("message cannot be retried")
	ErrNoRelay   = errors.New("relay is not configured")
	ErrNoClaim   = errors.New("no due message")
)

const (
	Pending  = "pending"
	Accepted = "accepted_by_upstream"
	Failed   = "failed"
	Expired  = "expired"
)

type Message struct {
	ID         string `gorm:"primaryKey" json:"id"`
	Raw        []byte `json:"-"`
	Sender     string `json:"sender"`
	Size       int64  `json:"size"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"`
	ClaimToken string `json:"-"`
	LeaseUntil int64  `json:"-"`
}

type Recipient struct {
	ID            string `gorm:"primaryKey" json:"id"`
	MessageID     string `json:"-"`
	Address       string `json:"address"`
	Status        string `json:"status"`
	Attempts      int    `json:"attempts"`
	NextAttemptAt int64  `json:"next_attempt_at"`
	LastError     string `json:"last_error"`
	CompletedAt   int64  `json:"completed_at,omitempty"`
}

type MessageStatus struct {
	Message

	Recipients []Recipient `json:"recipients"`
}

type Store struct {
	DB            *gorm.DB
	SQL           *sql.DB
	resource      *database.Database
	driver        string
	key           []byte
	fileRelay     *config.Relay
	writeFailures atomic.Int64
}

// New initializes the queue schema and takes ownership of the database resource.
func New(ctx context.Context, resource *database.Database, cfg config.Config, key []byte) (*Store, error) {
	sqlDB := resource.SQL()
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect(resource.Driver()); err != nil {
		_ = resource.Close()
		return nil, err
	}
	var err error
	if resource.Driver() == "postgres" {
		lock, lockErr := sqlDB.Conn(ctx)
		if lockErr != nil {
			_ = resource.Close()
			return nil, lockErr
		}
		_, err = lock.ExecContext(ctx, "SELECT pg_advisory_lock(782935011)")
		if err == nil {
			err = goose.UpContext(ctx, sqlDB, "migrations/postgres")
		}
		_, unlockErr := lock.ExecContext(ctx, "SELECT pg_advisory_unlock(782935011)")
		_ = lock.Close()
		if err == nil {
			err = unlockErr
		}
	} else {
		err = goose.UpContext(ctx, sqlDB, "migrations/sqlite")
	}
	if err != nil {
		_ = resource.Close()
		return nil, errors.New("database migration failed")
	}
	return &Store{DB: resource.DB(), SQL: sqlDB, resource: resource, driver: resource.Driver(), key: key, fileRelay: cfg.Relay}, nil
}

func (s *Store) Close() error { return s.resource.Close() }

func (s *Store) Ready(ctx context.Context) error { return s.SQL.PingContext(ctx) }

func (s *Store) dbNow() string {
	if s.driver == "postgres" {
		return "(extract(epoch from clock_timestamp()) * 1000)::bigint"
	}
	return "CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)"
}

func (s *Store) Enqueue(ctx context.Context, sender string, recipients []string, raw []byte, limits config.Limits, maxAge time.Duration) (string, error) {
	if len(recipients) == 0 || int64(len(raw)) > limits.MaxMessageBytes {
		return "", ErrQueueFull
	}
	now := time.Now().UnixMilli()
	id := uuid.NewString()
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Exec("UPDATE queue_capacity SET messages = messages + 1, bytes = bytes + ? WHERE id = 1 AND messages < ? AND bytes <= ?", len(raw), limits.MaxQueueMessages, limits.MaxQueueBytes-int64(len(raw)))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrQueueFull
		}
		message := Message{ID: id, Raw: raw, Sender: sender, Size: int64(len(raw)), CreatedAt: now, ExpiresAt: now + maxAge.Milliseconds()}
		if err := tx.Create(&message).Error; err != nil {
			return err
		}
		rows := make([]Recipient, 0, len(recipients))
		for _, address := range recipients {
			rows = append(rows, Recipient{ID: uuid.NewString(), MessageID: id, Address: address, Status: Pending, NextAttemptAt: now})
		}
		return tx.CreateInBatches(rows, 100).Error
	})
	if err != nil && !errors.Is(err, ErrQueueFull) {
		s.writeFailures.Add(1)
	}
	return id, err
}

type Claimed struct {
	Message

	Recipients []Recipient
}

func (s *Store) Claim(ctx context.Context, lease time.Duration) (*Claimed, error) {
	now := s.dbNow()
	var candidates []Message
	err := s.DB.WithContext(ctx).Select("id").Where("expires_at > "+now+" AND lease_until <= "+now+" AND EXISTS (SELECT 1 FROM recipients WHERE recipients.message_id = messages.id AND status = ? AND next_attempt_at <= "+now+")", Pending).Order("created_at, id").Limit(10).Find(&candidates).Error
	if err != nil {
		return nil, err
	}
	for _, candidate := range candidates {
		token := uuid.NewString()
		result := s.DB.WithContext(ctx).Model(&Message{}).Where("id = ? AND expires_at > "+now+" AND lease_until <= "+now+" AND EXISTS (SELECT 1 FROM recipients WHERE recipients.message_id = messages.id AND status = ? AND next_attempt_at <= "+now+")", candidate.ID, Pending).Updates(map[string]any{"claim_token": token, "lease_until": gorm.Expr(now+" + ?", lease.Milliseconds())})
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected != 1 {
			continue
		}
		var message Message
		if err := s.DB.WithContext(ctx).First(&message, "id = ?", candidate.ID).Error; err != nil {
			return nil, err
		}
		var recipients []Recipient
		if err := s.DB.WithContext(ctx).Where("message_id = ? AND status = ? AND next_attempt_at <= "+now, candidate.ID, Pending).Find(&recipients).Error; err != nil {
			return nil, err
		}
		return &Claimed{Message: message, Recipients: recipients}, nil
	}
	return nil, ErrNoClaim
}

type Result struct {
	ID     string
	Status string
	Error  string
	NextAt int64
}

func (s *Store) Complete(ctx context.Context, claim *Claimed, results []Result) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token string
		if err := tx.Model(&Message{}).Select("claim_token").Where("id = ? AND claim_token = ? AND lease_until > "+s.dbNow(), claim.ID, claim.ClaimToken).Scan(&token).Error; err != nil {
			return err
		}
		if token != claim.ClaimToken {
			return ErrConflict
		}
		for _, item := range results {
			updates := map[string]any{"status": item.Status, "next_attempt_at": item.NextAt, "last_error": item.Error, "attempts": gorm.Expr("attempts + 1")}
			if item.Status != Pending {
				updates["completed_at"] = time.Now().UnixMilli()
			}
			result := tx.Model(&Recipient{}).Where("id = ? AND message_id = ? AND status = ?", item.ID, claim.ID, Pending).Updates(updates)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrConflict
			}
		}
		result := tx.Model(&Message{}).Where("id = ? AND claim_token = ?", claim.ID, claim.ClaimToken).Updates(map[string]any{"claim_token": "", "lease_until": 0})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrConflict
		}
		return nil
	})
}

func (s *Store) Expire(ctx context.Context) error {
	now := s.dbNow()
	return s.DB.WithContext(ctx).Model(&Recipient{}).Where("status = ? AND message_id IN (SELECT id FROM messages WHERE expires_at <= "+now+")", Pending).Updates(map[string]any{"status": Expired, "completed_at": gorm.Expr(now), "last_error": "delivery window expired"}).Error
}

func (s *Store) Cleanup(ctx context.Context, retention time.Duration) error {
	cutoff := time.Now().Add(-retention).UnixMilli()
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []Message
		if err := tx.Select("id", "size").Where("created_at < ? AND NOT EXISTS (SELECT 1 FROM recipients WHERE recipients.message_id = messages.id AND (status = ? OR completed_at >= ?))", cutoff, Pending, cutoff).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			if err := tx.Where("message_id = ?", row.ID).Delete(&Recipient{}).Error; err != nil {
				return err
			}
			if err := tx.Delete(&row).Error; err != nil {
				return err
			}
			if err := tx.Exec("UPDATE queue_capacity SET messages = messages - 1, bytes = bytes - ? WHERE id = 1", row.Size).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) Retry(ctx context.Context, id string, maxAge, retention time.Duration) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		lock := tx.Exec("UPDATE messages SET lease_until = lease_until WHERE id = ?", id)
		if lock.Error != nil {
			return lock.Error
		}
		if lock.RowsAffected == 0 {
			return ErrNotFound
		}
		var active int64
		if err := tx.Model(&Recipient{}).Where("message_id = ? AND status = ?", id, Pending).Count(&active).Error; err != nil {
			return err
		}
		var inFlight int64
		if err := tx.Model(&Message{}).Where("id = ? AND lease_until > "+s.dbNow(), id).Count(&inFlight).Error; err != nil {
			return err
		}
		if active > 0 || inFlight > 0 {
			return ErrConflict
		}
		result := tx.Model(&Recipient{}).Where("message_id = ? AND status IN ? AND completed_at >= ?", id, []string{Failed, Expired}, time.Now().Add(-retention).UnixMilli()).Updates(map[string]any{"status": Pending, "next_attempt_at": time.Now().UnixMilli(), "last_error": "", "completed_at": 0})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		return tx.Model(&Message{}).Where("id = ?", id).Update("expires_at", time.Now().Add(maxAge).UnixMilli()).Error
	})
}

func (s *Store) List(ctx context.Context, status string, limit, offset int) ([]MessageStatus, error) {
	query := s.DB.WithContext(ctx).Model(&Message{}).Select("id", "sender", "size", "created_at", "expires_at")
	if status != "" {
		query = query.Where("EXISTS (SELECT 1 FROM recipients WHERE recipients.message_id = messages.id AND status = ?)", status)
	}
	var messages []Message
	if err := query.Order("created_at DESC, id DESC").Limit(limit).Offset(offset).Find(&messages).Error; err != nil {
		return nil, err
	}
	out := make([]MessageStatus, 0, len(messages))
	for _, message := range messages {
		var recipients []Recipient
		if err := s.DB.WithContext(ctx).Where("message_id = ?", message.ID).Order("address").Find(&recipients).Error; err != nil {
			return nil, err
		}
		out = append(out, MessageStatus{Message: message, Recipients: recipients})
	}
	return out, nil
}

type Metrics struct {
	Pending         int64
	Bytes           int64
	InFlight        int64
	Oldest          int64
	Accepted        int64
	Failed          int64
	Expired         int64
	Retries         int64
	DBWriteFailures int64
}

func (s *Store) Metrics(ctx context.Context) (Metrics, error) {
	var m Metrics
	m.DBWriteFailures = s.writeFailures.Load()
	rows, err := s.SQL.QueryContext(ctx, "SELECT status, COUNT(*), COALESCE(SUM(CASE WHEN attempts > 1 THEN attempts - 1 ELSE 0 END), 0) FROM recipients GROUP BY status")
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var status string
		var count, retries int64
		if err := rows.Scan(&status, &count, &retries); err != nil {
			_ = rows.Close()
			return m, err
		}
		m.Retries += retries
		switch status {
		case Pending:
			m.Pending = count
		case Accepted:
			m.Accepted = count
		case Failed:
			m.Failed = count
		case Expired:
			m.Expired = count
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return m, err
	}
	if err := rows.Close(); err != nil {
		return m, err
	}
	if err := s.SQL.QueryRowContext(ctx, "SELECT COALESCE(SUM(size), 0), COALESCE(MIN(created_at), 0) FROM messages WHERE EXISTS (SELECT 1 FROM recipients WHERE recipients.message_id = messages.id AND recipients.status = 'pending')").Scan(&m.Bytes, &m.Oldest); err != nil {
		return m, err
	}
	if err := s.SQL.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE lease_until > "+s.dbNow()).Scan(&m.InFlight); err != nil {
		return m, err
	}
	return m, nil
}

func ValidStatus(status string) bool {
	switch status {
	case "", Pending, Accepted, Failed, Expired:
		return true
	default:
		return false
	}
}
