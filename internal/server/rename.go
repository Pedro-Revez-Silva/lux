package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/marcioapm/lux/internal/spec"
	"github.com/marcioapm/lux/internal/store"
)

// PatchRunInput changes a Run's metadata: only its name for now.
type PatchRunInput struct {
	Name      runName `json:"name" required:"true" doc:"The new name: at most 200 bytes of UTF-8, no control characters; \"\" clears it. Also the stored spec's name."`
	decodeErr error
}

// UnmarshalJSON keeps a decode error (an unknown field, a non-string name)
// for patchRun to refuse as 422 invalid_spec.
func (in *PatchRunInput) UnmarshalJSON(data []byte) error {
	type plain PatchRunInput
	var decoded plain
	if err := decodeStrict(data, &decoded); err != nil {
		in.decodeErr = err
		return nil
	}
	*in = PatchRunInput(decoded)
	return nil
}

// runName is a JSON string that keeps invalid UTF-8 for CheckRunName to
// refuse (encoding/json would replace it with U+FFFD), and knows whether
// it was given: bodies are not validated against their schema. A null
// unsets it, so a duplicate key ending in null leaves no name.
type runName struct {
	value string
	set   bool
}

func (n *runName) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*n = runName{}
		return nil
	}
	if err := json.Unmarshal(data, &n.value); err != nil {
		return err
	}
	if !utf8.Valid(data) {
		n.value = string(data)
	}
	n.set = true
	return nil
}

// Schema is a string's: runName's fields are its own.
func (runName) Schema(huma.Registry) *huma.Schema { return &huma.Schema{Type: huma.TypeString} }

type patchRunInput struct {
	RunPath
	Body PatchRunInput
}

func (s *Server) patchRun(ctx context.Context, in *patchRunInput) (*runOutput, error) {
	p := principal(ctx)
	if in.Body.decodeErr != nil {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_spec", "invalid patch: %s", in.Body.decodeErr)
	}
	if !in.Body.Name.set {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_spec", "name is required")
	}
	to := in.Body.Name.value
	if err := spec.CheckRunName(to); err != nil {
		return nil, errf(http.StatusUnprocessableEntity, "invalid_spec", "%s", err.Error())
	}
	err := s.db.Tx(ctx, store.Tenant(p.TenantID), func(tx pgx.Tx) error {
		// The row lock orders a rename with the state changes and spec
		// rewrites that take it too (reports, resume, stop): each sees
		// the other's committed row.
		var from string
		if err := tx.QueryRow(ctx, `SELECT name FROM runs WHERE id = $1 FOR UPDATE`, in.ID).Scan(&from); err != nil {
			return err
		}
		if from == to {
			return nil
		}
		// The stored spec omits an empty name, as a submit without one stores it.
		if _, err := tx.Exec(ctx, `UPDATE runs SET name = $2, updated_at = now(),
				spec = CASE WHEN $2 = '' THEN spec - 'name' ELSE jsonb_set(spec, '{name}', to_jsonb($2::text)) END
			WHERE id = $1`, in.ID, to); err != nil {
			return err
		}
		return addEvent(ctx, tx, p.TenantID, in.ID, 0, "renamed", map[string]any{"by": p.Actor(), "from": from, "to": to})
	})
	if err != nil {
		return nil, err
	}
	return s.getRun(ctx, &in.RunPath)
}

// withNameProblem adds what is wrong with a submitted name to Normalize's
// problems (err), so a submit lists them all at once. An err that is not a
// ValidationError is returned as it is.
func withNameProblem(name string, err error) error {
	nameErr := spec.CheckRunName(name)
	if nameErr == nil {
		return err
	}
	problems := []string{nameErr.Error()}
	var normalized *spec.ValidationError
	switch {
	case errors.As(err, &normalized):
		problems = append(problems, normalized.Problems...)
	case err != nil:
		return err
	}
	return &spec.ValidationError{Problems: problems}
}
