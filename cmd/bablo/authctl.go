package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/starhui-dev/bablo/internal/auth"
	"github.com/starhui-dev/bablo/internal/config"
	"github.com/starhui-dev/bablo/internal/data"
)

func runAuthCommand(arguments []string) int {
	if len(arguments) == 0 {
		writeAuthUsage(os.Stderr)
		return 2
	}
	command := arguments[0]
	flags := flag.NewFlagSet("bablo auth "+command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	email := flags.String("email", "", "normalized account email")
	if err := flags.Parse(arguments[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*email) == "" {
		writeAuthUsage(os.Stderr)
		return 2
	}
	if command != "create-admin" && command != "reset-password" {
		writeAuthUsage(os.Stderr)
		return 2
	}

	password, err := readPassword(true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "password input error:", err)
		return 1
	}
	defer clearBytes(password)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		return 1
	}
	if cfg.DatabaseURL == "" {
		fmt.Fprintln(os.Stderr, "BABLO_DATABASE_URL is required")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := data.Open(ctx, data.Config{URL: cfg.DatabaseURL, MaxConns: 2})
	if err != nil {
		fmt.Fprintln(os.Stderr, "database connection error:", err)
		return 1
	}
	defer store.Close()
	repository, err := auth.NewRepository(store)
	if err != nil {
		fmt.Fprintln(os.Stderr, "authentication repository error:", err)
		return 1
	}
	service, err := auth.NewService(repository, auth.ServiceConfig{
		SessionTTL:      12 * time.Hour,
		Issuer:          "Bablo",
		RequireAdminMFA: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "authentication service error:", err)
		return 1
	}

	requestID := "authctl_" + time.Now().UTC().Format("20060102T150405.000000000Z")
	switch command {
	case "create-admin":
		user, err := service.CreateLocalUser(ctx, *email, string(password), true, requestID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create admin error:", publicCLIError(err))
			return 1
		}
		fmt.Fprintf(os.Stdout, "created admin %s (%s); enroll TOTP before using admin operations\n", user.Email, user.ID)
	case "reset-password":
		if err := service.LocalResetPassword(ctx, *email, string(password), requestID); err != nil {
			fmt.Fprintln(os.Stderr, "reset password error:", publicCLIError(err))
			return 1
		}
		fmt.Fprintf(os.Stdout, "reset password and revoked all sessions for %s\n", strings.ToLower(strings.TrimSpace(*email)))
	}
	return 0
}

func readPassword(confirm bool) ([]byte, error) {
	fileDescriptor := int(os.Stdin.Fd())
	if term.IsTerminal(fileDescriptor) {
		fmt.Fprint(os.Stderr, "Password: ")
		password, err := term.ReadPassword(fileDescriptor)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return nil, err
		}
		if !confirm {
			return password, nil
		}
		fmt.Fprint(os.Stderr, "Confirm password: ")
		confirmation, err := term.ReadPassword(fileDescriptor)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return nil, err
		}
		defer clearBytes(confirmation)
		if string(password) != string(confirmation) {
			clearBytes(password)
			return nil, errors.New("passwords do not match")
		}
		return password, nil
	}

	reader := bufio.NewReader(io.LimitReader(os.Stdin, 1026))
	password, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	password = bytesWithoutLineEnding(password)
	if len(password) > 1024 {
		clearBytes(password)
		return nil, errors.New("password exceeds 1024 bytes")
	}
	return password, nil
}

func bytesWithoutLineEnding(value []byte) []byte {
	if len(value) > 0 && value[len(value)-1] == '\n' {
		value = value[:len(value)-1]
	}
	if len(value) > 0 && value[len(value)-1] == '\r' {
		value = value[:len(value)-1]
	}
	return value
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func publicCLIError(err error) error {
	switch {
	case errors.Is(err, auth.ErrConflict):
		return errors.New("account already exists")
	case errors.Is(err, auth.ErrUserNotFound):
		return errors.New("account not found")
	case errors.Is(err, auth.ErrInvalidInput):
		return errors.New("email or password does not satisfy policy")
	default:
		slog.Error("authctl_error", "error", err)
		return errors.New("operation failed; inspect process logs")
	}
}

func writeAuthUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage:")
	fmt.Fprintln(writer, "  bablo auth create-admin --email user@example.com")
	fmt.Fprintln(writer, "  bablo auth reset-password --email user@example.com")
	fmt.Fprintln(writer, "password is read without echo from a terminal, or as one line from stdin")
}
