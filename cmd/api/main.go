package main

import (
	"log"

	"boggle-api/internal/server"
)

func main() {
	// Initialize the server and all dependencies
	srv := server.NewServer()
	
	// Ensure the database connection is closed when the application shuts down
	defer srv.Close()

	// Start the application
	if err := srv.Start(); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}
}