package main

import (
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/pkg/config"
	"github.com/eternisai/enchanted-proxy/pkg/invitecode"
	"github.com/eternisai/enchanted-proxy/pkg/storage/pg"
	"github.com/joho/godotenv"
)

func main() {
	var (
		customCode = flag.String("code", "", "Custom invite code (optional, generates random if not provided)")
		prefix     = flag.String("prefix", "", "Prefix for generated codes (e.g., BETA-, TEST-)")
		boundEmail = flag.String("email", "", "Bind code to specific email (optional)")
		expiryDays = flag.Int("expires", 0, "Expiry in days (0 = no expiry)")
		count      = flag.Int("count", 1, "Number of codes to generate")
		codeLength = flag.Int("length", 6, "Length of generated codes (default 6)")
		showHelp   = flag.Bool("help", false, "Show help")
	)
	flag.Parse()

	if *showHelp {
		fmt.Println("Invite Code Generator")
		fmt.Println("Usage: go run cmd/invite-generator/main.go [options]")
		fmt.Println("")
		fmt.Println("Options:")
		flag.PrintDefaults()
		fmt.Println("")
		fmt.Println("Examples:")
		fmt.Println("  go run cmd/invite-generator/main.go")
		fmt.Println("  go run cmd/invite-generator/main.go -code BETA25")
		fmt.Println("  go run cmd/invite-generator/main.go -prefix BETA- -count 5")
		fmt.Println("  go run cmd/invite-generator/main.go -email user@example.com -expires 30")
		fmt.Println("  go run cmd/invite-generator/main.go -length 8 -count 3")
		return
	}

	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using system environment variables")
	}

	config.LoadConfig()

	db, err := pg.InitDatabase(config.AppConfig.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.DB.Close() //nolint:errcheck

	service := invitecode.NewService(db.Queries)

	var expiresAt *time.Time
	if *expiryDays > 0 {
		expiry := time.Now().AddDate(0, 0, *expiryDays)
		expiresAt = &expiry
	}

	fmt.Printf("Generating %d invite code(s)...\n\n", *count)

	var generatedCodes []string

	for i := 0; i < *count; i++ {
		var code, codeHash string
		var err error

		if *customCode != "" && *count == 1 {
			code = *customCode
			codeHash = invitecode.HashCode(code)
		} else if *prefix != "" {
			code, err = invitecode.GenerateCodeWithPrefix(*prefix, *codeLength)
			if err != nil {
				log.Fatalf("Failed to generate code with prefix: %v", err)
			}
			codeHash = invitecode.HashCode(code)
		} else {
			code, err = invitecode.GenerateNanoIDWithLength(*codeLength)
			if err != nil {
				log.Fatalf("Failed to generate code: %v", err)
			}
			codeHash = invitecode.HashCode(code)
		}

		var boundEmailPtr *string
		if *boundEmail != "" {
			boundEmailPtr = boundEmail
		}

		inviteCode, err := service.CreateInviteCode(
			code,
			codeHash,
			boundEmailPtr,
			0,         // created_by (0 for system)
			false,     // is_used
			nil,       // redeemed_by
			nil,       // redeemed_at
			expiresAt, // expires_at
			true,      // is_active
		)

		if err != nil {
			log.Fatalf("Failed to create invite code: %v", err)
		}

		generatedCodes = append(generatedCodes, code)

		fmt.Printf("[%d/%d] %s\n", i+1, *count, code)
		fmt.Printf("      ID: %d\n", inviteCode.ID)

		if boundEmailPtr != nil {
			fmt.Printf("      Bound to: %s\n", *boundEmailPtr)
		}

		if expiresAt != nil {
			fmt.Printf("      Expires: %s\n", expiresAt.Format("2006-01-02 15:04:05"))
		} else {
			fmt.Printf("      Expires: Never\n")
		}

		fmt.Printf("      Created: %s\n", inviteCode.CreatedAt.Format("2006-01-02 15:04:05"))
		fmt.Println()
	}

	fmt.Printf("✅ Successfully generated %d invite code(s)\n\n", *count)

	fmt.Println("📋 Copy-paste list for spreadsheet:")
	fmt.Println(strings.Repeat("-", 40))
	for _, code := range generatedCodes {
		fmt.Println(code)
	}
	fmt.Println(strings.Repeat("-", 40))
}
