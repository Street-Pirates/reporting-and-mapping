package store

import (
	"path/filepath"
	"testing"

	"sightingmap/internal/db"
)

func TestStore_ListUsersAndSetUserRole(t *testing.T) {
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close()
	st := New(sqlDB)

	_, err = st.ResolveUser("test:user1")
	if err != nil {
		t.Fatalf("resolve user1: %v", err)
	}
	u2, err := st.ResolveUser("test:user2")
	if err != nil {
		t.Fatalf("resolve user2: %v", err)
	}

	users, err := st.ListUsers()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}

	for _, u := range users {
		switch u.OpaqueID {
		case users[0].OpaqueID, users[1].OpaqueID:
			if u.Role != "reporter" {
				t.Fatalf("expected reporter role for user %q, got %q", u.OpaqueID, u.Role)
			}
		default:
			t.Fatalf("unexpected user opaque_id %q", u.OpaqueID)
		}
	}

	if err := st.SetUserRole(users[1].OpaqueID, "administrator"); err != nil {
		t.Fatalf("set user role: %v", err)
	}

	updated, err := st.ListUsers()
	if err != nil {
		t.Fatalf("list users after update: %v", err)
	}
	var changed bool
	for _, u := range updated {
		if u.ID == u2.ID {
			changed = true
			if u.Role != "administrator" {
				t.Fatalf("expected administrator role for user %q, got %q", u.OpaqueID, u.Role)
			}
		}
	}
	if !changed {
		t.Fatal("updated user not found")
	}

	if err := st.SetUserRole("missing-opaque-id", "editor"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for missing user, got %v", err)
	}
}

func TestStore_SetUserCalled(t *testing.T) {
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close()
	st := New(sqlDB)

	u, err := st.ResolveUser("test:caller")
	if err != nil {
		t.Fatalf("resolve user: %v", err)
	}

	if err := st.SetUserCalled(t.Context(), u.ID, "hello"); err != nil {
		t.Fatalf("set user called: %v", err)
	}

	users, err := st.ListUsers()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(users))
	}
	if users[0].Called != "hello" {
		t.Fatalf("expected called=hello, got %q", users[0].Called)
	}
}

func TestStore_ListUserFlagsAndSetInsincere(t *testing.T) {
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close()
	st := New(sqlDB)

	target, err := st.ResolveUser("test:target")
	if err != nil {
		t.Fatalf("resolve target user: %v", err)
	}

	author, err := st.ResolveUser("test:author")
	if err != nil {
		t.Fatalf("resolve author user: %v", err)
	}

	if err := st.SetUserRole(author.OpaqueID, "administrator"); err != nil {
		t.Fatalf("set author role: %v", err)
	}

	if err := st.SetInsincere(t.Context(), author.ID, target.OpaqueID, true); err != nil {
		t.Fatalf("set insincere: %v", err)
	}

	flags, err := st.ListUserFlags(t.Context(), target.OpaqueID)
	if err != nil {
		t.Fatalf("list user flags: %v", err)
	}
	if len(flags) != 1 {
		t.Fatalf("expected 1 flag, got %d", len(flags))
	}
	if flags[0].FlagType != "insincere" {
		t.Fatalf("expected flag_type=insincere, got %q", flags[0].FlagType)
	}
	if flags[0].Value != 1 {
		t.Fatalf("expected value=1, got %d", flags[0].Value)
	}
	if flags[0].FlaggedByOpaqueID != author.OpaqueID {
		t.Fatalf("expected flagged_by=%q, got %q", author.OpaqueID, flags[0].FlaggedByOpaqueID)
	}

	if err := st.SetInsincere(t.Context(), author.ID, target.OpaqueID, false); err != nil {
		t.Fatalf("clear insincere: %v", err)
	}

	flags, err = st.ListUserFlags(t.Context(), target.OpaqueID)
	if err != nil {
		t.Fatalf("list user flags after clear: %v", err)
	}
	if len(flags) != 2 {
		t.Fatalf("expected 2 flags, got %d", len(flags))
	}
	if flags[0].Value != 0 {
		t.Fatalf("expected latest value=0 after clear, got %d", flags[0].Value)
	}
}
