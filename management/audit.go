package management

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// AuditResult classifies the outcome of an audited operation.
type AuditResult string

const (
	// AuditSuccess marks an operation that completed successfully.
	AuditSuccess AuditResult = "success"
	// AuditFailure marks an operation that failed.
	AuditFailure AuditResult = "failure"
)

// AuditEntry is one record of the operation audit log: who did what to
// which target, when, and with what outcome. Entries are append-only:
// Record assigns the ID and the (defaulted) timestamp, and no service
// mutates or removes individual entries — only Prune drops whole ranges.
type AuditEntry struct {
	ID       string      `json:"id"`
	Time     time.Time   `json:"time"`
	Operator string      `json:"operator,omitempty"`
	Action   string      `json:"action"`
	Target   string      `json:"target,omitempty"`
	Result   AuditResult `json:"result"`
	Detail   string      `json:"detail,omitempty"`
}

// AuditFilter selects audit entries by exact field match. An empty field
// does not constrain that dimension; an empty filter matches every entry.
type AuditFilter struct {
	Operator string
	Action   string
	Result   AuditResult
}

// AuditService provides append-only operation auditing over a Store.
type AuditService struct {
	store *Store
}

// NewAuditService returns an AuditService backed by store.
func NewAuditService(store *Store) *AuditService {
	return &AuditService{store: store}
}

// Record appends an entry to the audit log. The action must be non-empty
// (ErrInvalid) and a non-empty result must be a known result (ErrInvalid);
// an empty result defaults to AuditSuccess. A zero Time is replaced with
// the current time. It returns the recorded entry with its generated ID.
func (as *AuditService) Record(entry AuditEntry) (*AuditEntry, error) {
	if strings.TrimSpace(entry.Action) == "" {
		return nil, fmt.Errorf("audit: %w: empty action", ErrInvalid)
	}
	if entry.Result == "" {
		entry.Result = AuditSuccess
	}
	if err := validateAuditResult(entry.Result); err != nil {
		return nil, err
	}
	if entry.Time.IsZero() {
		entry.Time = time.Now()
	}
	e := &AuditEntry{
		ID:       newID(),
		Time:     entry.Time,
		Operator: entry.Operator,
		Action:   entry.Action,
		Target:   entry.Target,
		Result:   entry.Result,
		Detail:   entry.Detail,
	}
	err := as.store.Update(func(d *Data) error {
		d.AuditLogs = append(d.AuditLogs, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// List returns all audit entries, newest first (ordered by Time
// descending; the ID breaks ties for a deterministic order).
func (as *AuditService) List() []*AuditEntry {
	out := make([]*AuditEntry, 0)
	as.store.View(func(d *Data) {
		out = append(out, d.AuditLogs...)
	})
	sortAuditEntries(out)
	return out
}

// ListFiltered returns the audit entries matching filter, newest first.
// An empty Operator, Action or Result field in the filter does not
// constrain that dimension; matches are exact. It returns an empty list
// when no entry matches.
func (as *AuditService) ListFiltered(filter AuditFilter) []*AuditEntry {
	all := as.List()
	out := make([]*AuditEntry, 0, len(all))
	for _, e := range all {
		if filter.Operator != "" && e.Operator != filter.Operator {
			continue
		}
		if filter.Action != "" && e.Action != filter.Action {
			continue
		}
		if filter.Result != "" && e.Result != filter.Result {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Prune keeps only the newest maxEntries audit entries (ordered by Time
// descending, with the ID breaking ties) and removes the rest, returning
// the number removed. The surviving entries keep their original document
// order. A maxEntries <= 0 clears the entire audit log. When nothing is
// removed it is a no-op: the store is not rewritten and root UpdatedAt is
// not bumped.
func (as *AuditService) Prune(maxEntries int) (int, error) {
	removed := 0
	err := as.store.Update(func(d *Data) error {
		if maxEntries <= 0 {
			if len(d.AuditLogs) == 0 {
				return errNoop
			}
			removed = len(d.AuditLogs)
			d.AuditLogs = d.AuditLogs[:0]
			return nil
		}
		if len(d.AuditLogs) <= maxEntries {
			return errNoop
		}
		sorted := make([]*AuditEntry, len(d.AuditLogs))
		copy(sorted, d.AuditLogs)
		sortAuditEntries(sorted)
		keep := make(map[*AuditEntry]bool, maxEntries)
		for _, e := range sorted[:maxEntries] {
			keep[e] = true
		}
		kept := d.AuditLogs[:0]
		for _, e := range d.AuditLogs {
			if keep[e] {
				kept = append(kept, e)
			}
		}
		d.AuditLogs = kept
		removed = len(sorted) - maxEntries
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// sortAuditEntries orders entries newest first: Time descending, with the
// ID breaking ties (ascending) for a deterministic order.
func sortAuditEntries(entries []*AuditEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Time.Equal(entries[j].Time) {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Time.After(entries[j].Time)
	})
}

func validateAuditResult(r AuditResult) error {
	switch r {
	case AuditSuccess, AuditFailure:
		return nil
	}
	return fmt.Errorf("audit: %w: unknown result %q", ErrInvalid, r)
}
