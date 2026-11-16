package main

import "testing"

func TestExcludeLocalResourcePeers(t *testing.T) {
	manager := taskManagerLabel()
	peers := []peerView{
		{ID: "peer/aaaaaaaa/forward/web", Manager: manager},
		{ID: "peer/bbbbbbbb/forward/web", Manager: manager + "-other"},
		{ID: "peer/cccccccc/mount/web", Manager: manager},
		{ID: "peer/dddddddd/forward/other", Manager: manager},
	}
	got := excludeLocalResourcePeers(peers, []resourceRecord{{Kind: "forward", Name: "web"}})
	if len(got) != 3 || got[0].Manager != manager+"-other" || got[1].ID != "peer/cccccccc/mount/web" || got[2].ID != "peer/dddddddd/forward/other" {
		t.Fatalf("remaining peers: %#v", got)
	}
}
