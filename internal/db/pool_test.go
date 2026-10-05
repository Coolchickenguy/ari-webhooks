package db

import "testing"

func TestSanitizeUrl(t *testing.T) {
	got := SanitizeUrl("postgres://user:pw@localhost:5432/ari?schema=public&connection_limit=5&sslmode=disable")
	if got != "postgres://user:pw@localhost:5432/ari?sslmode=disable" {
		t.Fatalf("prisma params must be stripped, pgx params kept: %s", got)
	}
	if got := SanitizeUrl("postgres://localhost/ari"); got != "postgres://localhost/ari" {
		t.Fatalf("plain url untouched: %s", got)
	}
}
