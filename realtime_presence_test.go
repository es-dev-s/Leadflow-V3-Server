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
