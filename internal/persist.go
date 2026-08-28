package internal

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// persister records in-flight author refreshes so we can recover them on reboot.
type persister interface {
	Persist(ctx context.Context, authorID int64, current []byte) error
	Persisted(ctx context.Context) ([]int64, error)
	Delete(ctx context.Context, authorID int64) error
}

// Persister tracks author refresh state across reboots.
type Persister struct {
	db    *pgxpool.Pool
	cache cache[[]byte]
}

// nopersist no-ops persistence for tests.
type nopersist struct{}

var (
	_ persister = (*Persister)(nil)
	_ persister = (*nopersist)(nil)
)

func (*nopersist) Persist(ctx context.Context, authorID int64, current []byte) error {
	return nil
}

func (*nopersist) Persisted(ctx context.Context) ([]int64, error) {
	return nil, nil
}

func (*nopersist) Delete(ctx context.Context, authorID int64) error {
	return nil
}

// NewPersister creates a new Persister.
func NewPersister(ctx context.Context, cache cache[[]byte], dsn string) (*Persister, error) {
	db, err := newDB(ctx, dsn)
	return &Persister{db: db, cache: cache}, err
}

// Persist records an author's refresh as in-flight.
func (p *Persister) Persist(ctx context.Context, authorID int64, bytes []byte) error {
	p.cache.Set(ctx, refreshAuthorKey(authorID), bytes, 365*24*time.Hour)
	return nil
}

// Delete records an in-flight refresh as completed.
func (p *Persister) Delete(ctx context.Context, authorID int64) error {
	Log(ctx).Info("finished loading author", "authorID", authorID)
	return p.cache.Delete(ctx, refreshAuthorKey(authorID))
}

// Persisted returns all in-flight author refreshes so they can be resumed. IDs
// are returned in FIFO order.
func (p *Persister) Persisted(ctx context.Context) ([]int64, error) {
	start := time.Now()

	// A range predicate can use the cache primary-key index. LIKE 'ra%' caused
	// a sequential scan on large metadata caches during startup recovery.
	rows, err := p.db.Query(ctx, "SELECT SUBSTRING(key, 3), expires FROM cache WHERE key >= 'ra' AND key < 'rb'")
	if err != nil {
		Log(ctx).Error("unable to recover in-flight refreshes", "err", err)
		return nil, err
	}
	defer rows.Close()

	type persistedAuthor struct {
		id      int64
		expires time.Time
	}
	items := make([]persistedAuthor, 0)

	for rows.Next() {
		var id string
		var expires pgtype.Timestamptz
		err := rows.Scan(&id, &expires)
		if err != nil {
			continue
		}
		if authorID, err := strconv.ParseInt(id, 10, 64); err == nil {
			items = append(items, persistedAuthor{id: authorID, expires: expires.Time})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading persisted author refreshes: %w", err)
	}

	// Do not key by expiration timestamp: distinct rows can share a timestamp,
	// and the old map silently dropped all but one of them. Sort the records
	// directly and use the author ID as a deterministic tie-breaker.
	slices.SortFunc(items, func(a, b persistedAuthor) int {
		if ordered := a.expires.Compare(b.expires); ordered != 0 {
			return ordered
		}
		return cmp.Compare(a.id, b.id)
	})
	authorIDs := make([]int64, 0, len(items))
	seen := make(map[int64]struct{}, len(items))
	for _, item := range items {
		if _, ok := seen[item.id]; ok {
			continue
		}
		seen[item.id] = struct{}{}
		authorIDs = append(authorIDs, item.id)
	}

	if len(authorIDs) > 0 {
		Log(ctx).Debug("recovered in-flight refreshes", "count", len(authorIDs), "duration", time.Since(start).String())
	}

	return authorIDs, nil
}

func refreshAuthorKey(authorID int64) string {
	return fmt.Sprintf("ra%d", authorID)
}
