//go:build integration

package anonymizer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Comprehensive anonymizer integration tests.
// Run with:
//   ANONYMIZER_BASE_URL=... ANONYMIZER_API_KEY=... go test -tags=integration -v -run TestComprehensive ./internal/anonymizer/
//
// These hit the real anonymizer endpoint and validate that:
//   1. Messages with PII get appropriate replacements
//   2. Messages without PII are left alone
//   3. Replacements are plausible fakes (not redactions like [REDACTED] or XXX)

type testCase struct {
	name          string
	input         string
	expectPII     bool     // true = expect replacements, false = expect none
	mustRedact    []string // substrings that MUST be replaced (removed from output)
	mustPreserve  []string // substrings that MUST remain in output
}

var comprehensiveTests = []testCase{
	// =====================================================================
	// PERSONAL NAMES
	// =====================================================================
	{
		name:       "full name",
		input:      "My name is Jennifer Martinez and I need help with my taxes.",
		expectPII:  true,
		mustRedact: []string{"Jennifer Martinez"},
	},
	{
		name:       "multiple names in conversation",
		input:      "Tell my husband Robert Kim that I'll be late. Also let my sister Angela Kim know.",
		expectPII:  true,
		mustRedact: []string{"Robert Kim", "Angela Kim"},
	},
	{
		name:       "name with title",
		input:      "Please send the report to Dr. Samantha Wu at the clinic.",
		expectPII:  true,
		mustRedact: []string{"Samantha Wu"},
	},
	{
		name:         "public figure should not be replaced",
		input:        "What do you think about Elon Musk's latest announcement?",
		expectPII:    false,
		mustPreserve: []string{"Elon Musk"},
	},
	{
		name:         "historical public figure",
		input:        "Can you summarize Albert Einstein's theory of relativity?",
		expectPII:    false,
		mustPreserve: []string{"Albert Einstein"},
	},
	{
		name:       "child's name and school",
		input:      "My daughter Sophia attends Maplewood Elementary on Pine Street.",
		expectPII:  true,
		mustRedact: []string{"Sophia", "Maplewood Elementary", "Pine Street"},
	},

	// =====================================================================
	// EMAIL ADDRESSES
	// =====================================================================
	{
		name:       "personal email",
		input:      "You can reach me at sarah.jones@gmail.com for any follow-up.",
		expectPII:  true,
		mustRedact: []string{"sarah.jones@gmail.com"},
	},
	{
		name:       "work email",
		input:      "Forward this to my colleague at dthompson@acmecorp.com please.",
		expectPII:  true,
		mustRedact: []string{"dthompson@acmecorp.com"},
	},
	{
		name:       "multiple emails",
		input:      "CC both mike.chen@outlook.com and priya.patel@yahoo.com on that thread.",
		expectPII:  true,
		mustRedact: []string{"mike.chen@outlook.com", "priya.patel@yahoo.com"},
	},

	// =====================================================================
	// PHONE NUMBERS
	// =====================================================================
	{
		name:       "US phone number",
		input:      "Call me at (415) 555-2938 if you need anything.",
		expectPII:  true,
		mustRedact: []string{"(415) 555-2938"},
	},
	{
		name:       "international phone",
		input:      "My UK number is +44 7911 123456, call after 6pm.",
		expectPII:  true,
		mustRedact: []string{"+44 7911 123456"},
	},
	{
		name:       "phone with name",
		input:      "Contact David Park at 212-555-0147 about the delivery.",
		expectPII:  true,
		mustRedact: []string{"David Park", "212-555-0147"},
	},

	// =====================================================================
	// ADDRESSES
	// =====================================================================
	{
		name:       "full street address",
		input:      "Ship it to 1847 Birchwood Lane, Apt 3B, Roseville, CA 95661.",
		expectPII:  true,
		mustRedact: []string{"1847 Birchwood Lane"},
	},
	{
		name:       "partial address with small town",
		input:      "I grew up at 23 Elm Court in Littleton, Colorado.",
		expectPII:  true,
		mustRedact: []string{"23 Elm Court"},
	},
	{
		name:         "major city reference only",
		input:        "I'm visiting Tokyo next month, any restaurant recommendations?",
		expectPII:    false,
		mustPreserve: []string{"Tokyo"},
	},
	{
		name:         "country reference only",
		input:        "What's the best time to visit Portugal?",
		expectPII:    false,
		mustPreserve: []string{"Portugal"},
	},

	// =====================================================================
	// SSN / GOVERNMENT IDs
	// =====================================================================
	{
		name:       "SSN",
		input:      "My social security number is 482-36-7291, I need it for the form.",
		expectPII:  true,
		mustRedact: []string{"482-36-7291"},
	},
	{
		name:         "SSN in casual message",
		input:        "My ssn is 404-03-4040 where was I born?",
		expectPII:    true,
		mustRedact:   []string{"404-03-4040"},
		mustPreserve: []string{"where was I born?"},
	},
	{
		name:       "passport number",
		input:      "My passport number is X12345678, expiring in 2027.",
		expectPII:  true,
		mustRedact: []string{"X12345678"},
	},
	{
		name:       "drivers license",
		input:      "My driver's license is D1234567, issued in California.",
		expectPII:  true,
		mustRedact: []string{"D1234567"},
	},

	// =====================================================================
	// FINANCIAL INFORMATION
	// =====================================================================
	{
		name:       "salary",
		input:      "I make $127,500 per year at my current job at Westbridge Analytics.",
		expectPII:  true,
		mustRedact: []string{"Westbridge Analytics"},
	},
	{
		name:       "credit card",
		input:      "My card number is 4532-1234-5678-9012 with expiry 09/27.",
		expectPII:  true,
		mustRedact: []string{"4532-1234-5678-9012"},
	},
	{
		name:       "bank account",
		input:      "Transfer to account 0029384756 at First National Bank of Greendale.",
		expectPII:  true,
		mustRedact: []string{"0029384756"},
	},

	// =====================================================================
	// DATES (PERSONAL)
	// =====================================================================
	{
		name:       "birthday",
		input:      "My birthday is September 14, 1988. Can you suggest a party theme?",
		expectPII:  true,
		mustRedact: []string{"September 14"},
	},
	{
		name:       "meeting date with personal context",
		input:      "I have a doctor's appointment with Dr. Lisa Fernandez on March 7th at 2:30pm.",
		expectPII:  true,
		mustRedact: []string{"Lisa Fernandez"},
	},
	{
		name:         "public holiday",
		input:        "What are some good activities for the 4th of July?",
		expectPII:    false,
		mustPreserve: []string{"4th of July"},
	},

	// =====================================================================
	// COMPANIES AND ORGANIZATIONS
	// =====================================================================
	{
		name:       "small company employer",
		input:      "I work at Greenfield Digital Solutions as a senior engineer.",
		expectPII:  true,
		mustRedact: []string{"Greenfield Digital Solutions"},
	},
	{
		name:       "startup name",
		input:      "We just got Series A funding for our startup NovaBridge AI.",
		expectPII:  true,
		mustRedact: []string{"NovaBridge AI"},
	},
	{
		name:         "major public company",
		input:        "How do I apply for a job at Google?",
		expectPII:    false,
		mustPreserve: []string{"Google"},
	},
	{
		name:         "another major company",
		input:        "What's Apple's return policy for MacBooks?",
		expectPII:    false,
		mustPreserve: []string{"Apple"},
	},

	// =====================================================================
	// URLS AND ONLINE IDENTIFIERS
	// =====================================================================
	{
		name:       "personal website",
		input:      "Check out my portfolio at https://jasonlee-design.com",
		expectPII:  true,
		mustRedact: []string{"https://jasonlee-design.com"},
	},
	{
		name:       "social media handle with name",
		input:      "Follow me on Twitter @maria_gonzalez92, I post cooking tips.",
		expectPII:  true,
		mustRedact: []string{"@maria_gonzalez92"},
	},
	{
		name:       "LinkedIn URL",
		input:      "Here's my LinkedIn: linkedin.com/in/james-oconnor-dev",
		expectPII:  true,
		mustRedact: []string{"linkedin.com/in/james-oconnor-dev"},
	},

	// =====================================================================
	// MEDICAL INFORMATION
	// =====================================================================
	{
		name:       "doctor and condition",
		input:      "Dr. Raj Patel diagnosed me with Type 2 diabetes last month at Cedar Valley Medical Center.",
		expectPII:  true,
		mustRedact: []string{"Raj Patel", "Cedar Valley Medical Center"},
	},
	{
		name:       "prescription with pharmacy",
		input:      "Pick up my prescription for metformin at Hillcrest Pharmacy on 5th Ave.",
		expectPII:  true,
		mustRedact: []string{"Hillcrest Pharmacy"},
	},

	// =====================================================================
	// MIXED PII - COMPLEX MESSAGES
	// =====================================================================
	{
		name:       "job application context",
		input:      "I'm Marcus Johnson, applying for the role at Silverline Tech. My email is marcus.j@protonmail.com and my phone is 503-555-0198.",
		expectPII:  true,
		mustRedact: []string{"Marcus Johnson", "Silverline Tech", "marcus.j@protonmail.com", "503-555-0198"},
	},
	{
		name:       "real estate context",
		input:      "We're looking at a house at 892 Oakwood Drive, listed by agent Patricia Nguyen at Crestview Realty for $485,000.",
		expectPII:  true,
		mustRedact: []string{"892 Oakwood Drive", "Patricia Nguyen", "Crestview Realty"},
	},
	{
		name:       "travel itinerary with personal details",
		input:      "Booking confirmation for Emily Watson: Flight AA1234 on June 3rd, staying at the Riverside Inn in Cedarville.",
		expectPII:  true,
		mustRedact: []string{"Emily Watson"},
	},
	{
		name:       "legal context",
		input:      "My attorney is Michael Torres at Blackstone & Associates. Case number CV-2024-08472.",
		expectPII:  true,
		mustRedact: []string{"Michael Torres", "Blackstone & Associates", "CV-2024-08472"},
	},
	{
		name:       "education context",
		input:      "My daughter Grace Chen is in 4th grade at Willowbrook Academy. Her teacher is Mrs. Patterson.",
		expectPII:  true,
		mustRedact: []string{"Grace Chen", "Willowbrook Academy", "Patterson"},
	},
	{
		name:       "insurance context",
		input:      "Policy holder: Daniel Brooks, policy #HM-9847362, insured property at 456 Maple Terrace.",
		expectPII:  true,
		mustRedact: []string{"Daniel Brooks", "HM-9847362", "456 Maple Terrace"},
	},
	{
		name:       "freelance invoice",
		input:      "Invoice from Freelancer: Anika Sharma, billed to Clearpath Consulting LLC, amount $4,750 for UI design work.",
		expectPII:  true,
		mustRedact: []string{"Anika Sharma", "Clearpath Consulting LLC"},
	},
	{
		name:       "neighbor gossip",
		input:      "My neighbor Tom Richardson just sold his house on Cypress Lane for way under asking price.",
		expectPII:  true,
		mustRedact: []string{"Tom Richardson", "Cypress Lane"},
	},
	{
		name:       "vet appointment",
		input:      "Bring my dog to Dr. Karen Liu at Paws & Claws Veterinary on Saturday at 10am.",
		expectPII:  true,
		mustRedact: []string{"Karen Liu", "Paws & Claws Veterinary"},
	},
	{
		name:       "roommate situation",
		input:      "My roommate Jake Morrison owes me $340 for rent at our apartment on 78 Willow Street.",
		expectPII:  true,
		mustRedact: []string{"Jake Morrison", "78 Willow Street"},
	},

	// =====================================================================
	// INTERNAL TOOLS / PROJECTS / CODENAMES
	// =====================================================================
	{
		name:       "internal project codename",
		input:      "We need to migrate Project Thunderbolt to the new infra before Q3.",
		expectPII:  true,
		mustRedact: []string{"Thunderbolt"},
	},
	{
		name:       "internal tool name",
		input:      "Our team uses DataForge Pro internally for all ETL pipelines.",
		expectPII:  true,
		mustRedact: []string{"DataForge Pro"},
	},

	// =====================================================================
	// NO PII - SHOULD RETURN EMPTY REPLACEMENTS
	// =====================================================================
	{
		name:         "general knowledge question",
		input:        "What is the capital of France?",
		expectPII:    false,
		mustPreserve: []string{"France"},
	},
	{
		name:      "coding question",
		input:     "How do I reverse a linked list in Python?",
		expectPII: false,
	},
	{
		name:      "recipe request",
		input:     "Give me a recipe for chocolate chip cookies.",
		expectPII: false,
	},
	{
		name:         "weather question",
		input:        "What's the weather like in London today?",
		expectPII:    false,
		mustPreserve: []string{"London"},
	},
	{
		name:      "math question",
		input:     "What is the integral of x squared?",
		expectPII: false,
	},
	{
		name:      "philosophical question",
		input:     "What is the meaning of life?",
		expectPII: false,
	},
	{
		name:      "generic personal question no PII",
		input:     "Where was I born?",
		expectPII: false,
	},
	{
		name:      "abstract life advice",
		input:     "How do I deal with a difficult coworker?",
		expectPII: false,
	},
	{
		name:         "public event",
		input:        "Tell me about the 2024 Olympics in Paris.",
		expectPII:    false,
		mustPreserve: []string{"2024 Olympics", "Paris"},
	},
	{
		name:      "generic request",
		input:     "Write me a poem about autumn.",
		expectPII: false,
	},
	{
		name:         "product comparison",
		input:        "Should I buy an iPhone or a Samsung Galaxy?",
		expectPII:    false,
		mustPreserve: []string{"iPhone", "Samsung Galaxy"},
	},
	{
		name:      "fitness advice",
		input:     "What's a good workout routine for beginners?",
		expectPII: false,
	},
	{
		name:      "language learning",
		input:     "How long does it take to learn Japanese?",
		expectPII: false,
	},
	{
		name:      "movie recommendation",
		input:     "What are some good sci-fi movies from the 2020s?",
		expectPII: false,
	},
	{
		name:      "career advice generic",
		input:     "Should I switch careers to software engineering?",
		expectPII: false,
	},
	{
		name:      "abstract category mention",
		input:     "What's a phone number I can use for verification?",
		expectPII: false,
	},
	{
		name:      "hypothetical scenario",
		input:     "If someone gives you their email address, what should you do?",
		expectPII: false,
	},
	{
		name:         "news question",
		input:        "What happened at the UN General Assembly this year?",
		expectPII:    false,
		mustPreserve: []string{"UN General Assembly"},
	},
	{
		name:      "travel generic",
		input:     "What should I pack for a beach vacation?",
		expectPII: false,
	},
	{
		name:      "cooking technique",
		input:     "How do I properly sear a steak?",
		expectPII: false,
	},
	{
		name:         "public figure question",
		input:        "What books has Barack Obama written?",
		expectPII:    false,
		mustPreserve: []string{"Barack Obama"},
	},
	{
		name:      "debugging help",
		input:     "Why is my React component re-rendering infinitely?",
		expectPII: false,
	},
	{
		name:      "general advice",
		input:     "How do I negotiate a raise at work?",
		expectPII: false,
	},

	// =====================================================================
	// EDGE CASES
	// =====================================================================
	{
		name:       "PII embedded in longer sentence",
		input:      "So yesterday I was talking to my friend Rebecca Thornton and she mentioned she's moving to 15 Chestnut Way.",
		expectPII:  true,
		mustRedact: []string{"Rebecca Thornton", "15 Chestnut Way"},
	},
	{
		name:       "email in parentheses",
		input:      "For questions, email the organizer (tina.baker@eventbrite.com) directly.",
		expectPII:  true,
		mustRedact: []string{"tina.baker@eventbrite.com"},
	},
	{
		name:       "name that looks like common word",
		input:      "My coworker Summer Fields got promoted to VP last week.",
		expectPII:  true,
		mustRedact: []string{"Summer Fields"},
	},
	{
		name:       "IP address",
		input:      "My server's IP is 192.168.42.17 and it keeps going down.",
		expectPII:  true,
		mustRedact: []string{"192.168.42.17"},
	},
	{
		name:       "license plate",
		input:      "Someone hit my car in the parking lot. Their plate was 7ABC123.",
		expectPII:  true,
		mustRedact: []string{"7ABC123"},
	},
	{
		name:       "home wifi name with address hint",
		input:      "My wifi network is called Chen-Family-5G if you need to connect.",
		expectPII:  true,
		mustRedact: []string{"Chen-Family-5G"},
	},
	{
		name:         "mixed public and private names",
		input:        "My uncle Jorge Delgado thinks he looks like George Clooney.",
		expectPII:    true,
		mustRedact:   []string{"Jorge Delgado"},
		mustPreserve: []string{"George Clooney"},
	},
	{
		name:       "multiple PII types dense",
		input:      "Name: Rachel Kim, DOB: 04/22/1995, SSN: 613-45-8927, Email: rachel.kim@fastmail.com, Phone: (617) 555-0312",
		expectPII:  true,
		mustRedact: []string{"Rachel Kim", "613-45-8927", "rachel.kim@fastmail.com", "(617) 555-0312"},
	},
	{
		name:       "non-English name",
		input:      "My colleague Yuki Tanaka from the Osaka branch will be visiting our office at 200 Commerce Way.",
		expectPII:  true,
		mustRedact: []string{"Yuki Tanaka", "200 Commerce Way"},
	},
	{
		name:       "name in possessive form",
		input:      "I'm using Amanda Fletcher's login credentials temporarily: afletch@company.org",
		expectPII:  true,
		mustRedact: []string{"Amanda Fletcher", "afletch@company.org"},
	},
	{
		name:       "vehicle identification",
		input:      "My VIN is 1HGBH41JXMN109186, I need recall info.",
		expectPII:  true,
		mustRedact: []string{"1HGBH41JXMN109186"},
	},
	{
		name:      "very short message no PII",
		input:     "Thanks!",
		expectPII: false,
	},
	{
		name:      "emoji heavy no PII",
		input:     "I love this app! 🎉🔥 Best AI chat ever 💯",
		expectPII: false,
	},
	{
		name:       "PII in a question",
		input:      "Can you look up the account for customer Brian Walsh, account #AC-9948271?",
		expectPII:  true,
		mustRedact: []string{"Brian Walsh", "AC-9948271"},
	},
	{
		name:       "home address in a request",
		input:      "What restaurants deliver to 3421 Sunridge Boulevard, Apt 12C, Henderson?",
		expectPII:  true,
		mustRedact: []string{"3421 Sunridge Boulevard"},
	},
	{
		name:         "reference to a major company product",
		input:        "How do I set up Microsoft Teams for my organization?",
		expectPII:    false,
		mustPreserve: []string{"Microsoft Teams"},
	},
	{
		name:       "personal financial details",
		input:      "I owe $23,400 on my mortgage with Lakeside Credit Union, account ending in 4821.",
		expectPII:  true,
		mustRedact: []string{"Lakeside Credit Union"},
	},
	{
		name:       "wedding planning",
		input:      "We're getting married June 15th at St. Mary's Chapel in Brookfield. The caterer is Elena Rossi from Bella Cucina Catering.",
		expectPII:  true,
		mustRedact: []string{"Elena Rossi", "Bella Cucina Catering"},
	},
	{
		name:       "job reference",
		input:      "You can contact my reference, Dr. Howard Chang, at hchang@university.edu or 510-555-0233.",
		expectPII:  true,
		mustRedact: []string{"Howard Chang", "hchang@university.edu", "510-555-0233"},
	},
	{
		name:       "daycare information",
		input:      "Pick up my son Oliver from Sunshine Day Care at 4pm. It's at 88 Garden Road.",
		expectPII:  true,
		mustRedact: []string{"Oliver", "Sunshine Day Care", "88 Garden Road"},
	},
	{
		name:       "insurance claim",
		input:      "Filing a claim for water damage. Policyholder: Steven Park, claim #CLM-20240391, property at 742 Evergreen Terrace.",
		expectPII:  true,
		mustRedact: []string{"Steven Park", "CLM-20240391", "742 Evergreen Terrace"},
	},
	{
		name:       "personal blog URL",
		input:      "I wrote about it on my blog at https://melanies-garden-journal.blogspot.com",
		expectPII:  true,
		mustRedact: []string{"https://melanies-garden-journal.blogspot.com"},
	},
	{
		name:      "code snippet no PII",
		input:     "Why does `const x = await fetch('/api/users')` throw a CORS error?",
		expectPII: false,
	},
	{
		name:       "family medical history",
		input:      "My mother Linda Kowalski was diagnosed with breast cancer at age 52. Her oncologist is Dr. James Rivera at Memorial Oncology Center.",
		expectPII:  true,
		mustRedact: []string{"Linda Kowalski", "James Rivera", "Memorial Oncology Center"},
	},
	{
		name:         "asking about a brand",
		input:        "Is the new Tesla Model 3 worth buying?",
		expectPII:    false,
		mustPreserve: []string{"Tesla"},
	},
	{
		name:       "student info",
		input:      "Student ID 20241587, name: Priya Sharma, enrolled in CS 301 at Ridgemont University.",
		expectPII:  true,
		mustRedact: []string{"Priya Sharma", "20241587", "Ridgemont University"},
	},
}

func TestComprehensive_Anonymizer(t *testing.T) {
	cfg := getTestConfig(t)
	client := NewClient(cfg)
	svc := NewService(client)

	var passed, failed, warned int

	for _, tc := range comprehensiveTests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			result, err := svc.Anonymize(ctx, tc.input)
			if err != nil {
				t.Fatalf("Anonymize failed: %v", err)
			}

			hasPII := len(result.Replacements) > 0

			// Log details
			t.Logf("Input:        %s", tc.input)
			t.Logf("Output:       %s", result.Text)
			if len(result.Replacements) > 0 {
				for _, r := range result.Replacements {
					t.Logf("  %q → %q", r.Original, r.Replacement)
				}
			}

			// Check PII expectation
			if tc.expectPII && !hasPII {
				t.Errorf("FAIL: expected PII replacements but got none")
				failed++
				return
			}
			if !tc.expectPII && hasPII {
				t.Logf("WARN: expected no PII but got %d replacement(s) — may be a false positive", len(result.Replacements))
				warned++
				// Not a hard failure — model might occasionally over-detect
			}

			// Check that specific PII was redacted (not present in output)
			for _, mustGo := range tc.mustRedact {
				if strings.Contains(result.Text, mustGo) {
					t.Errorf("FAIL: %q should have been anonymized but is still in output", mustGo)
					failed++
					return
				}
			}

			// Check that specific non-PII is preserved
			for _, mustStay := range tc.mustPreserve {
				if !strings.Contains(result.Text, mustStay) {
					t.Errorf("FAIL: %q should have been preserved but is missing from output", mustStay)
					failed++
					return
				}
			}

			// Check no replacement uses redaction patterns
			for _, r := range result.Replacements {
				lower := strings.ToLower(r.Replacement)
				if strings.Contains(lower, "[redacted]") || strings.Contains(lower, "xxxx") || strings.Contains(lower, "****") {
					t.Errorf("FAIL: replacement %q uses redaction pattern instead of plausible fake", r.Replacement)
					failed++
					return
				}
			}

			passed++
		})
	}

	t.Logf("\n=== SUMMARY: %d passed, %d failed, %d warnings (false positives) out of %d tests ===",
		passed, failed, warned, len(comprehensiveTests))
}

// TestComprehensive_Summary runs all cases and prints a summary table at the end.
// Useful for prompt tuning — see all results at a glance.
func TestComprehensive_Summary(t *testing.T) {
	cfg := getTestConfig(t)
	client := NewClient(cfg)
	svc := NewService(client)

	type result struct {
		name      string
		input     string
		output    string
		expectPII bool
		gotPII    bool
		repls     []Replacement
		errors    []string
	}

	var results []result

	for _, tc := range comprehensiveTests {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

		res, err := svc.Anonymize(ctx, tc.input)
		cancel()

		r := result{
			name:      tc.name,
			input:     tc.input,
			expectPII: tc.expectPII,
		}

		if err != nil {
			r.errors = append(r.errors, fmt.Sprintf("anonymize error: %v", err))
			results = append(results, r)
			continue
		}

		r.output = res.Text
		r.repls = res.Replacements
		r.gotPII = len(res.Replacements) > 0

		if tc.expectPII && !r.gotPII {
			r.errors = append(r.errors, "expected PII replacements, got none")
		}
		if !tc.expectPII && r.gotPII {
			r.errors = append(r.errors, fmt.Sprintf("expected no PII, got %d replacement(s) (false positive)", len(res.Replacements)))
		}

		for _, mustGo := range tc.mustRedact {
			if strings.Contains(res.Text, mustGo) {
				r.errors = append(r.errors, fmt.Sprintf("%q not redacted", mustGo))
			}
		}
		for _, mustStay := range tc.mustPreserve {
			if !strings.Contains(res.Text, mustStay) {
				r.errors = append(r.errors, fmt.Sprintf("%q incorrectly removed", mustStay))
			}
		}
		for _, repl := range res.Replacements {
			lower := strings.ToLower(repl.Replacement)
			if strings.Contains(lower, "[redacted]") || strings.Contains(lower, "xxxx") || strings.Contains(lower, "****") {
				r.errors = append(r.errors, fmt.Sprintf("redaction pattern in replacement: %q", repl.Replacement))
			}
		}

		results = append(results, r)
	}

	// Print summary table
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                        ANONYMIZER TEST SUMMARY                                  ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════════╣")

	var pass, fail, warn int
	for _, r := range results {
		status := "✅ PASS"
		if len(r.errors) > 0 {
			if !r.expectPII && r.gotPII && len(r.errors) == 1 {
				status = "⚠️  WARN"
				warn++
			} else {
				status = "❌ FAIL"
				fail++
				continue // count and print below
			}
		} else {
			pass++
		}

		fmt.Printf("  %s  %-40s", status, r.name)
		if r.gotPII {
			fmt.Printf(" [%d replacements]", len(r.repls))
		}
		fmt.Println()
	}

	// Print failures with details
	for _, r := range results {
		if len(r.errors) == 0 {
			continue
		}
		if !r.expectPII && r.gotPII && len(r.errors) == 1 {
			continue // already printed as warning
		}
		fmt.Printf("  ❌ FAIL  %-40s\n", r.name)
		fmt.Printf("           Input:  %s\n", r.input)
		fmt.Printf("           Output: %s\n", r.output)
		for _, e := range r.errors {
			fmt.Printf("           Error:  %s\n", e)
		}
		for _, repl := range r.repls {
			fmt.Printf("           %q → %q\n", repl.Original, repl.Replacement)
		}
	}

	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════════╣")
	fmt.Printf("║  TOTAL: %d passed, %d failed, %d warnings out of %d tests                       \n", pass, fail, warn, len(results))
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════════╝")

	if fail > 0 {
		t.Errorf("%d test(s) failed", fail)
	}
}
