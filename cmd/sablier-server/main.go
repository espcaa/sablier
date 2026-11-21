package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"
	_ "time/tzdata"

	"github.com/espcaa/sablier/internal/server"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()

	c, err := server.ConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx, c); err != nil {
		log.Fatal(err)
	}
}
