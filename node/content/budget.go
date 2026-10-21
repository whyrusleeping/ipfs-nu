package content

import (
	"errors"
	"fmt"

	"github.com/whyrusleeping/nu"
	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/store"
)

// InsufficientStorage is the insufficient_storage error for a store refusal (D17 A6):
// the message says what did not fit, the action how to make room, and details carry the
// machine-readable shortfall {reason, need, headroom, physical, limit, free, reserve}.
func InsufficientStorage(be *store.BudgetError) *apiv0.Error {
	var msg, action string
	switch be.Reason {
	case "free_space":
		msg = fmt.Sprintf("not enough disk space: %s needed, %s available above the %s free-space reserve",
			sizeString(be.Need), sizeString(be.Headroom), sizeString(be.Reserve))
		action = "free disk space, release content (`" + nu.Name + " pin rm`), or lower store.free_reserve, then run the command again; nothing pinned is removed automatically"
	default:
		msg = fmt.Sprintf("store limit reached: %s needed, %s left of the %s limit",
			sizeString(be.Need), sizeString(be.Headroom), sizeString(be.Limit))
		action = "release content (`" + nu.Name + " pin rm`) or raise store.limit (`" + nu.Name + " config set store.limit ...`), then run the command again; nothing pinned is removed automatically"
	}
	e := apiv0.Errorf(apiv0.CodeInsufficientStorage, "%s", msg).WithAction(action)
	for k, v := range map[string]int64{"need": be.Need, "headroom": be.Headroom, "physical": be.Physical,
		"limit": be.Limit, "free": be.Free, "reserve": be.Reserve} {
		e = e.WithDetail(k, v)
	}
	return e.WithDetail("reason", be.Reason)
}

// StoreError maps errors the store returns into the API error contract; other errors
// pass through unchanged.
func StoreError(err error) error {
	var be *store.BudgetError
	if errors.As(err, &be) {
		return InsufficientStorage(be)
	}
	return err
}

func sizeString(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
