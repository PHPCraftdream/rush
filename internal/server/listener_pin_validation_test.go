package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
)

type pinValidationSessionService struct {
	session.Service
	err error
}

func (s pinValidationSessionService) Get(context.Context, string) (session.Session, error) {
	return session.Session{}, s.err
}

func TestValidateEffectivePinsToleratesOnlyMissingRecordSession(t *testing.T) {
	databaseErr := errors.New("database unavailable")
	contextErr := context.Canceled
	cases := []struct {
		name    string
		err     error
		wantErr bool
	}{
		{name: "session not found", err: sql.ErrNoRows},
		{name: "database error", err: databaseErr, wantErr: true},
		{name: "context error", err: contextErr, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &appPkg.App{Sessions: pinValidationSessionService{err: tc.err}}
			tokens := []config.APIToken{{
				ID:      "token-id",
				Binding: config.APITokenBinding{Kind: "session", Pin: &config.APITokenPinBinding{Session: "session-id"}},
			}}
			err := ValidateEffectivePins(context.Background(), app, tokens, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected lookup error to refuse startup")
				}
				if !errors.Is(err, tc.err) {
					t.Fatalf("error %v does not wrap %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("missing record pin should be tolerated: %v", err)
			}
		})
	}
}

func TestValidateEffectivePinsWrapsUnexpectedLookupError(t *testing.T) {
	lookupErr := fmt.Errorf("wrapped database error: %w", errors.New("disk failure"))
	app := &appPkg.App{Sessions: pinValidationSessionService{err: lookupErr}}
	token := config.APIToken{ID: "token-id", Binding: config.APITokenBinding{Kind: "session", Pin: &config.APITokenPinBinding{Session: "session-id"}}}
	err := ValidateEffectivePins(context.Background(), app, []config.APIToken{token}, nil, nil)
	if !errors.Is(err, lookupErr) {
		t.Fatalf("error %v does not wrap lookup error %v", err, lookupErr)
	}
}
