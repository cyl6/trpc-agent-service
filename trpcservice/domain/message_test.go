package domain

import "testing"

func TestIdentityIsolationAndConversationRules(t *testing.T) {
	base := InboundMessage{
		TenantID: "tenant-a", Channel: "telegram", ExternalUserID: "u1",
		ConversationID: "chat-1", Scope: ScopeDirect,
	}
	userA, directA := Identity(base, "assistant")

	otherChat := base
	otherChat.ConversationID = "chat-2"
	_, directB := Identity(otherChat, "assistant")
	if directA != directB {
		t.Fatalf("direct chats for one user should share a session: %s != %s", directA, directB)
	}

	group := base
	group.Scope = ScopeGroup
	groupUserA, groupA := Identity(group, "assistant")
	otherSender := group
	otherSender.ExternalUserID = "u2"
	groupUserB, sameGroup := Identity(otherSender, "assistant")
	if groupUserA != groupUserB || groupA != sameGroup {
		t.Fatal("members of one group must share the runner principal and session")
	}
	group.ConversationID = "chat-2"
	_, groupB := Identity(group, "assistant")
	if groupA == groupB {
		t.Fatal("different groups must not share a session")
	}

	otherTenant := base
	otherTenant.TenantID = "tenant-b"
	userB, sessionB := Identity(otherTenant, "assistant")
	if userA == userB || directA == sessionB {
		t.Fatal("tenant must be part of both user and session identities")
	}
}

func TestAppNamespaceIsStableAndOpaque(t *testing.T) {
	a := AppNamespace("tenant-a", "assistant")
	b := AppNamespace("tenant-b", "assistant")
	if a == b || len(a) != 35 {
		t.Fatalf("unexpected namespaces %q %q", a, b)
	}
}

func TestThreadParticipatesInSessionIdentity(t *testing.T) {
	message := InboundMessage{
		TenantID: "tenant-a", BindingID: "main", Channel: "slack",
		ExternalUserID: "u1", ConversationID: "channel-1", Scope: ScopeGroup,
	}
	_, root := Identity(message, "assistant")
	message.ThreadID = "thread-1"
	_, thread := Identity(message, "assistant")
	if root == thread {
		t.Fatal("root conversation and explicit thread must not share a session")
	}
}
