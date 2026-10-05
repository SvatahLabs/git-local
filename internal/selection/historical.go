package selection

import (
	"context"
	"errors"
)

// PlanHistorical admits missing paths only with types supplied by an admitted
// snapshot. Existing paths still undergo ordinary kind, ownership and safety checks.
func (e *Engine) PlanHistorical(ctx context.Context, s State, rules []Rule) (Plan, error) {
	if len(rules) == 0 {
		return Plan{}, errors.New("at least one historical path is required")
	}
	old, err := Canonical(s)
	if err != nil {
		return Plan{}, err
	}
	next := old
	for _, rule := range rules {
		next.Generation = old.Generation
		next, err = next.Track(rule)
		if err != nil {
			return Plan{}, err
		}
	}
	next.Generation = old.Generation
	next, err = changed(old, next)
	if err != nil {
		return Plan{}, err
	}
	return e.Validate(ctx, next)
}
