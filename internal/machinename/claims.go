package machinename

import (
	"errors"
	"fmt"
	"sort"

	"github.com/shaul/mesh/internal/identity"
)

var ErrReplay = errors.New("destination reported an older machine name revision")
var ErrEquivocation = errors.New("destination reported another machine name at the same revision")

func ValidateClaim(owner string, claim Claim) error {
	if claim.ID != owner {
		return ErrTarget
	}
	if _, err := identity.IdentityKey(owner); err != nil {
		return fmt.Errorf("machine name claim identity: %w", err)
	}
	name, err := Normalize(claim.MachineName)
	if err != nil {
		return err
	}
	if name != claim.MachineName || claim.Revision == 0 {
		return errors.New("machine name claim requires a canonical name and nonzero revision")
	}
	return nil
}

func acceptClaim(current, next Claim) (bool, error) {
	if current.Revision > next.Revision {
		return false, ErrReplay
	}
	if current.Revision == next.Revision {
		if current.MachineName != next.MachineName {
			return false, ErrEquivocation
		}
		return false, nil
	}
	return true, nil
}

type Projection struct {
	Claim
	Conflict bool
	Priority bool
	Suffix   string
}

// Project orders a validated, unique-ID claim set without changing any owner's name.
func Project(claims []Claim) []Projection {
	rows := make([]Projection, len(claims))
	for i, claim := range claims {
		suffix := claim.ID
		if len(suffix) > 8 {
			suffix = suffix[len(suffix)-8:]
		}
		rows[i] = Projection{Claim: claim, Suffix: suffix}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].MachineName != rows[j].MachineName {
			return rows[i].MachineName < rows[j].MachineName
		}
		return rows[i].ID < rows[j].ID
	})
	for i := range rows {
		previous := i > 0 && rows[i-1].MachineName == rows[i].MachineName
		next := i+1 < len(rows) && rows[i+1].MachineName == rows[i].MachineName
		rows[i].Priority = !previous
		rows[i].Conflict = previous || next
	}
	return rows
}
