package main

import "testing"

func TestRealtimeHubPresenceUniqueUsers(t *testing.T) {
	hub := NewRealtimeHub()
	a1 := &sseClient{id: "c1", userID: "se-1", role: RoleSalesExecutive, teamID: "team-a", ch: make(chan []byte, 8)}
	a2 := &sseClient{id: "c2", userID: "se-1", role: RoleSalesExecutive, teamID: "team-a", ch: make(chan []byte, 8)}
	b1 := &sseClient{id: "c3", userID: "admin-1", role: RoleSuperadmin, ch: make(chan []byte, 8)}

	_, first := hub.add(a1)
	if !first {
		t.Fatal("first tab for SE should be firstForUser")
	}
	_, first = hub.add(a2)
	if first {
		t.Fatal("second tab for same SE must not count as a new presence")
	}
	hub.add(b1)

	if !hub.IsOnline("se-1") {
		t.Fatal("sales executive should be online with a live SSE tab")
	}
	if hub.OnlineCount("") != 2 {
		t.Fatalf("expected 2 unique online users, got %d", hub.OnlineCount(""))
	}
	if hub.OnlineCount("team-a") != 1 {
		t.Fatalf("expected 1 online user on team-a, got %d", hub.OnlineCount("team-a"))
	}

	if _, last := hub.remove("c1"); last {
		t.Fatal("removing one of two tabs must keep the user online")
	}
	if !hub.IsOnline("se-1") {
		t.Fatal("sales executive should stay online while another tab is connected")
	}
	if _, last := hub.remove("c2"); !last {
		t.Fatal("removing the last tab should mark the user offline")
	}
	if hub.IsOnline("se-1") {
		t.Fatal("sales executive should be offline after the last tab disconnects")
	}
}

func TestDropUserDoesNotKickOtherAccounts(t *testing.T) {
	hub := NewRealtimeHub()
	a := &sseClient{id: "a1", userID: "user-a", sid: "sid-a", ch: make(chan []byte, 8)}
	b := &sseClient{id: "b1", userID: "user-b", sid: "sid-b", ch: make(chan []byte, 8)}
	hub.add(a)
	hub.add(b)

	hub.DropUser("user-a")
	if hub.IsOnline("user-a") {
		t.Fatal("replaced account must be dropped")
	}
	if !hub.IsOnline("user-b") {
		t.Fatal("a login on another account must not drop this user")
	}

	keep := &sseClient{id: "a2", userID: "user-a", sid: "sid-new", ch: make(chan []byte, 8)}
	old := &sseClient{id: "a3", userID: "user-a", sid: "sid-old", ch: make(chan []byte, 8)}
	hub.add(keep)
	hub.add(old)
	hub.DropUserExcept("user-a", "sid-new")
	if hub.clientCount() != 2 {
		t.Fatalf("expected live session + other account, got %d clients", hub.clientCount())
	}
	if !hub.IsOnline("user-a") {
		t.Fatal("new session SSE must stay connected")
	}
	if !hub.IsOnline("user-b") {
		t.Fatal("other account must stay online after DropUserExcept")
	}
}
