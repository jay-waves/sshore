//go:build linux || windows

package files

import (
	"errors"
	"testing"
	"time"

	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
)

func TestMountRequestSchedulerReservesMetadataCapacity(t *testing.T) {
	s := &mountRequestScheduler{ctx: t.Context(), timeout: 20 * time.Millisecond}
	largeRead := &sshfx.ReadPacket{Length: MountSFTPDataLength}
	var releases []func()
	for range mountRequestWindows[mountData] {
		sent, release, err := s.admit(nil, nil, largeRead)
		if err != nil {
			t.Fatal(err)
		}
		sent()
		releases = append(releases, release)
	}

	for _, packet := range []sshfx.PacketMarshaller{&sshfx.StatPacket{}, &sshfx.ReadPacket{Length: 16}} {
		sent, release, err := s.admit(nil, nil, packet)
		if err != nil {
			t.Fatalf("interactive request was blocked behind the full data window: %v", err)
		}
		sent()
		release()
	}
	if _, _, err := s.admit(nil, nil, &sshfx.WritePacket{}); !errors.Is(err, errMountRequestQueueTimeout) {
		t.Fatalf("full data window should time out a queued write, got %v", err)
	}
	for _, release := range releases {
		release()
	}
	sent, release, err := s.admit(nil, nil, largeRead)
	if err != nil {
		t.Fatalf("queued timeout leaked a slot: %v", err)
	}
	sent()
	release()
}

func TestMountRequestSchedulerAlternatesEligibleQueues(t *testing.T) {
	s := &mountRequestScheduler{sending: true}
	m1, m2, d1, d2 := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.queues[mountMetadata].PushBack(m1)
	s.queues[mountMetadata].PushBack(m2)
	s.queues[mountData].PushBack(d1)
	s.queues[mountData].PushBack(d2)
	pending := []chan struct{}{m1, d1, m2, d2}
	for i, expected := range pending {
		s.sending = false
		s.grant()
		select {
		case <-expected:
		default:
			t.Fatalf("grant %d did not alternate queues and preserve their arrival order", i)
		}
		for _, later := range pending[i+1:] {
			select {
			case <-later:
				t.Fatal("granted more than one writer")
			default:
			}
		}
	}
}
