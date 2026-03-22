# Gin Boggle API

This project is a Boggle game API built with Go, using the Gin web framework. It provides endpoints to manage game rooms and handle real-time gameplay via WebSockets.

## Project Structure

- `cmd/api/` - Main entry point for the API server
- `internal/handler/` - HTTP and WebSocket handlers
- `internal/models/` - Data models
- `internal/repository/` - Database access logic
- `internal/server/` - Server setup and routing
- `internal/service/` - Business logic
- `migrations/` - Database migration scripts

## Setup Instructions

1. **Clone the repository**
   ```sh
   git clone <your-repo-url>
   cd <repo-directory>
   ```

2. **Start the development environment**
   - Open the project in VS Code.
   - If prompted, reopen in the dev container.

3. **Install dependencies**
   ```sh
   go mod tidy
   ```

4. **Run database migrations**
   - Ensure PostgreSQL is running (the dev container includes `postgresql-client`).
   - Apply migrations using your preferred tool or manually with `psql`:
     ```sh
     psql $DATABASE_URL -f migrations/001_create_rooms.sql
     ```

5. **Run the API server**
   ```sh
   go run ./cmd/api/main.go
   ```

6. **API will be available at** [http://localhost:8080](http://localhost:8080)

## Debugging

- Use the "Debug Boggle API" configuration in VS Code to start debugging with breakpoints.

## Environment Variables

- `DATABASE_URL` (see `.vscode/launch.json` for an example)

---

For more details, see the code in [`cmd/api/main.go`](cmd/api/main.go) and related internal packages.