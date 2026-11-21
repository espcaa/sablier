package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/espcaa/sablier/internal/api"
	"github.com/espcaa/sablier/internal/config"
	"github.com/espcaa/sablier/internal/store"
)

const (
	syncEvery   = 2 * time.Minute
	finalSyncIn = 5 * time.Second // exit works quickly even when you are offline
)

func startAutoSync(s *store.Store, every time.Duration, warn io.Writer) (stop func()) {
	c, err := config.Load()
	if err != nil || c.APIKey == "" || s == nil {
		return func() {}
	}
	client := api.New(c.APIURL, c.APIKey)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			upload(ctx, s, client, io.Discard) // errors are retried next round, and the agent owns the screen
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	return func() {
		cancel()
		<-done
		ctx, cancel := context.WithTimeout(context.Background(), finalSyncIn)
		defer cancel()
		if _, err := upload(ctx, s, client, warn); err != nil {
			fmt.Fprintln(warn, "sablier: couldn't sync, will retry next time:", err)
		}
	}
}
