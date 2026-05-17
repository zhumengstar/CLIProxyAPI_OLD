package auth

import (
	"net/http"
	"testing"
	"time"
)

func TestShouldDeleteInvalidAuthFilePermanentAuthFailure(t *testing.T) {
	t.Parallel()

	auth := &Auth{
		ID: "expired",
		LastError: &Error{
			Message:    "unauthorized",
			HTTPStatus: http.StatusUnauthorized,
		},
	}

	if !shouldDeleteInvalidAuthFile(auth, time.Now()) {
		t.Fatal("expected permanent authentication failure to be deleted")
	}
}

func TestShouldDeleteInvalidAuthFileDeletesQuotaOnlyAuth(t *testing.T) {
	t.Parallel()

	auth := &Auth{
		ID:     "quota",
		Status: StatusError,
		Quota:  QuotaState{Exceeded: true, Reason: "quota"},
	}

	if !shouldDeleteInvalidAuthFile(auth, time.Now()) {
		t.Fatal("expected quota-only auth to be deleted")
	}
}

func TestShouldDeleteInvalidAuthFileKeepsDisabledAuth(t *testing.T) {
	t.Parallel()

	auth := &Auth{
		ID:       "disabled",
		Disabled: true,
		LastError: &Error{
			Message:    "unauthorized",
			HTTPStatus: http.StatusUnauthorized,
		},
	}

	if shouldDeleteInvalidAuthFile(auth, time.Now()) {
		t.Fatal("expected intentionally disabled auth to be kept")
	}
}
