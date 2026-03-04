import Foundation

// MARK: - Configuration

struct Message {
    let role: String
    let content: String
}

struct TestCase {
    let name: String
    let messages: [Message]
    let maxTokens: Int
    let sentinel: String

    /// Convenience for single-prompt test cases
    init(name: String, prompt: String, maxTokens: Int, sentinel: String = "THE END") {
        self.name = name
        self.messages = [Message(role: "user", content: prompt)]
        self.maxTokens = maxTokens
        self.sentinel = sentinel
    }

    init(name: String, messages: [Message], maxTokens: Int, sentinel: String = "THE END") {
        self.name = name
        self.messages = messages
        self.maxTokens = maxTokens
        self.sentinel = sentinel
    }
}

let sentinel = "THE END"
let minContentLength = 100

let testCases: [TestCase] = [
    // 1. Short response
    TestCase(
        name: "short",
        prompt: "Say hello and introduce yourself in 2-3 sentences. End your response with \(sentinel) on its own line.",
        maxTokens: 300
    ),

    // 2. Medium response
    TestCase(
        name: "medium",
        prompt: "Write a haiku about the ocean, then explain its meaning in 3-4 sentences. End your response with \(sentinel) on its own line.",
        maxTokens: 500
    ),

    // 3. Long creative writing
    TestCase(
        name: "long-story",
        prompt: "Write a creative story of approximately 500 words about a robot discovering music for the first time. End your story with \(sentinel) on its own line.",
        maxTokens: 2000
    ),

    // 4. Reasoning-heavy
    TestCase(
        name: "reasoning",
        prompt: "What is the sum of the first 20 prime numbers? Show your reasoning step by step, then give the final answer. End your response with \(sentinel) on its own line.",
        maxTokens: 2000
    ),

    // 5. Long essay
    TestCase(
        name: "long-essay",
        prompt: "Write a detailed essay of approximately 1000 words about the history of artificial intelligence, from its earliest concepts to modern day. End your essay with \(sentinel) on its own line.",
        maxTokens: 4000
    ),

    // 6. Multi-turn conversation
    TestCase(
        name: "multi-turn",
        messages: [
            Message(role: "user", content: "Hello, how are you?"),
            Message(role: "assistant", content: "I'm doing well, thank you for asking! How are you today?"),
            Message(role: "user", content: "Great, thanks! Now please write a detailed essay about trigonometry. Cover the basics (sine, cosine, tangent), the unit circle, common identities, and real-world applications. Make it around 2000 words and don't make any mistakes. End your essay with \(sentinel) on its own line."),
        ],
        maxTokens: 6000
    ),

    // 7. Code generation (different content type)
    TestCase(
        name: "code",
        prompt: "Write a Python function that implements a binary search tree with insert, search, and delete operations. Include docstrings and comments. End your response with \(sentinel) on its own line.",
        maxTokens: 3000
    ),

    // 8. Short multi-turn (control — should succeed if short responses work)
    TestCase(
        name: "multi-turn-short",
        messages: [
            Message(role: "user", content: "What is 2+2?"),
            Message(role: "assistant", content: "2 + 2 = 4."),
            Message(role: "user", content: "Good. Now what is 10+10? Give just the answer, then end with \(sentinel) on its own line."),
        ],
        maxTokens: 300
    ),
]

// MARK: - Endpoint configuration

struct Endpoint {
    let name: String
    let url: String
    let model: String
}

let directEndpoint = Endpoint(
    name: "Direct (NEAR AI)",
    url: "https://cloud-api.near.ai/v1/chat/completions",
    model: "zai-org/GLM-5-FP8"
)

let proxyEndpoint = Endpoint(
    name: "Via Proxy (localhost)",
    url: "http://localhost:12000/chat/completions",
    model: "glm-5"
)

// MARK: - Load .env

func loadEnvValue(_ key: String) -> String? {
    if let val = ProcessInfo.processInfo.environment[key], !val.isEmpty {
        return val
    }
    var dir = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
    for _ in 0..<5 {
        let envFile = dir.appendingPathComponent(".env")
        if FileManager.default.fileExists(atPath: envFile.path),
           let contents = try? String(contentsOf: envFile, encoding: .utf8) {
            for line in contents.components(separatedBy: .newlines) {
                let trimmed = line.trimmingCharacters(in: .whitespaces)
                if trimmed.hasPrefix("\(key)=") {
                    return String(trimmed.dropFirst("\(key)=".count))
                }
            }
        }
        dir = dir.deletingLastPathComponent()
    }
    return nil
}

func makeTestJWT() -> String {
    func b64url(_ s: String) -> String {
        Data(s.utf8).base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }
    let header = b64url(#"{"alg":"none","typ":"JWT"}"#)
    let payload = b64url(#"{"sub":"test-user","exp":4102444800}"#)
    return "\(header).\(payload)."
}

// MARK: - Raw SSE fetcher

struct SSEChunk {
    let lineNumber: Int
    let raw: String
    let json: [String: Any]?
}

func fetchRawSSE(endpoint: Endpoint, apiKey: String, testCase: TestCase) async throws -> (chunks: [SSEChunk], httpStatus: Int) {
    var request = URLRequest(url: URL(string: endpoint.url)!)
    request.httpMethod = "POST"
    request.setValue("Bearer \(apiKey)", forHTTPHeaderField: "Authorization")
    request.setValue("application/json", forHTTPHeaderField: "Content-Type")
    request.timeoutInterval = 300

    let messages = testCase.messages.map { msg -> [String: Any] in
        ["role": msg.role, "content": msg.content]
    }

    let body: [String: Any] = [
        "model": endpoint.model,
        "messages": messages,
        "stream": true,
        "max_tokens": testCase.maxTokens,
    ]
    request.httpBody = try JSONSerialization.data(withJSONObject: body)

    let (asyncBytes, response) = try await URLSession.shared.bytes(for: request)
    let httpStatus = (response as? HTTPURLResponse)?.statusCode ?? 0

    var chunks: [SSEChunk] = []
    var lineNumber = 0

    for try await line in asyncBytes.lines {
        lineNumber += 1
        guard line.hasPrefix("data: ") else { continue }
        let dataStr = String(line.dropFirst(6))

        if dataStr == "[DONE]" {
            chunks.append(SSEChunk(lineNumber: lineNumber, raw: line, json: nil))
            continue
        }

        var parsed: [String: Any]?
        if let data = dataStr.data(using: .utf8) {
            parsed = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
        }
        chunks.append(SSEChunk(lineNumber: lineNumber, raw: line, json: parsed))
    }

    return (chunks, httpStatus)
}

// MARK: - Test result

enum TestVerdict: String {
    case pass = "✅ PASS"
    case partial = "⚠️  PARTIAL"   // stream worked but content incomplete
    case fail = "❌ FAIL"          // stream error or no content
}

struct TestResult {
    let name: String
    let verdict: TestVerdict
    let checks: [(passed: Bool, message: String)]
    let contentTail: String     // last N chars of content for inspection
    let hadStreamError: Bool    // upstream error was normalized
}

// MARK: - Run one test

func runOneTest(endpoint: Endpoint, apiKey: String, testCase: TestCase, outputPrefix: String) async -> TestResult {
    var checks: [(Bool, String)] = []
    func pass(_ msg: String) { checks.append((true, "✓ \(msg)")) }
    func fail(_ msg: String) { checks.append((false, "✗ \(msg)")) }
    func info(_ msg: String) { checks.append((true, "ℹ \(msg)")) }

    let msgDesc = testCase.messages.count > 1
        ? "\(testCase.messages.count) messages (multi-turn)"
        : String(testCase.messages.last!.content.prefix(80)) + "..."
    info("Prompt: \(msgDesc)")

    do {
        let (chunks, httpStatus) = try await fetchRawSSE(
            endpoint: endpoint, apiKey: apiKey, testCase: testCase
        )

        // Save raw SSE
        let rawSSE = chunks.map(\.raw).joined(separator: "\n")
        writeFile("\(outputPrefix)/\(testCase.name).sse", rawSSE)

        guard httpStatus == 200 else {
            fail("HTTP \(httpStatus)")
            return TestResult(name: testCase.name, verdict: .fail, checks: checks, contentTail: "", hadStreamError: false)
        }

        // Analyze chunks
        var content = ""
        var reasoning = ""
        var finishReason: String?
        var parseFailures: [(line: Int, raw: String)] = []
        var hadStreamError = false

        for chunk in chunks {
            if chunk.raw.contains("[DONE]") { continue }

            guard let json = chunk.json else {
                parseFailures.append((chunk.lineNumber, chunk.raw))
                continue
            }

            if let choices = json["choices"] as? [[String: Any]], let choice = choices.first {
                if let fr = choice["finish_reason"] as? String {
                    finishReason = fr
                }
                if let delta = choice["delta"] as? [String: Any] {
                    if let c = delta["content"] as? String {
                        content += c
                    }
                    // Check both field names (direct has reasoning_content, proxy has reasoning)
                    if let r = delta["reasoning_content"] as? String {
                        reasoning += r
                    }
                    if let r = delta["reasoning"] as? String {
                        reasoning += r
                    }
                }
            }
        }

        // Detect normalized stream errors in content
        if content.contains("[Stream error:") {
            hadStreamError = true
        }

        // --- Checks ---

        // 1. Parse failures (non-JSON lines)
        if !parseFailures.isEmpty {
            for failure in parseFailures.prefix(2) {
                fail("Non-JSON SSE line L\(failure.line): \(String(failure.raw.prefix(100)))")
            }
        }

        // 2. Content produced
        if !content.isEmpty {
            pass("Content: \(content.count) chars")
        } else if !reasoning.isEmpty {
            fail("No content produced (reasoning only: \(reasoning.count) chars)")
        } else {
            fail("No content or reasoning produced")
        }

        // 3. Reasoning (informational)
        if !reasoning.isEmpty {
            info("Reasoning: \(reasoning.count) chars")
        }

        // 4. Stream error
        if hadStreamError {
            fail("Upstream stream error (normalized by proxy)")
        }

        // 5. finish_reason
        if finishReason == "stop" {
            pass("finish_reason: stop")
        } else if finishReason == "length" {
            fail("finish_reason: length — hit token limit before completing")
        } else if let fr = finishReason {
            info("finish_reason: \(fr)")
        } else {
            fail("No finish_reason — stream ended abnormally")
        }

        // 6. Sentinel check — THE END must appear in content
        let contentLower = content.lowercased()
        let sentinelLower = testCase.sentinel.lowercased()
        var sentinelFound = false
        if contentLower.contains(sentinelLower) {
            // Make sure it's not just from the [Stream error:] injection
            let contentWithoutError = content.components(separatedBy: "\n\n[Stream error:").first ?? content
            if contentWithoutError.lowercased().contains(sentinelLower) {
                pass("Sentinel \"\(testCase.sentinel)\" found in content")
                sentinelFound = true
            } else {
                fail("Sentinel \"\(testCase.sentinel)\" NOT found — only in error suffix")
            }
        } else {
            fail("Sentinel \"\(testCase.sentinel)\" NOT found — response incomplete")
        }

        // 7. Minimum content length (skip if sentinel was found — short correct answers are fine)
        if !sentinelFound {
            if content.count >= minContentLength {
                pass("Content length \(content.count) >= \(minContentLength) min")
            } else {
                fail("Content too short: \(content.count) chars (min \(minContentLength))")
            }
        }

        // Content tail for inspection
        let tail = String(content.suffix(200))

        // Determine verdict
        let failCount = checks.filter { !$0.0 }.count
        let verdict: TestVerdict
        if failCount == 0 {
            verdict = .pass
        } else if !content.isEmpty && content.count >= minContentLength {
            verdict = .partial
        } else {
            verdict = .fail
        }

        return TestResult(name: testCase.name, verdict: verdict, checks: checks, contentTail: tail, hadStreamError: hadStreamError)

    } catch {
        fail("Error: \(error.localizedDescription)")
        return TestResult(name: testCase.name, verdict: .fail, checks: checks, contentTail: "", hadStreamError: false)
    }
}

// MARK: - Output helpers

func writeFile(_ path: String, _ content: String) {
    let url = URL(fileURLWithPath: path)
    try? FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
    try? content.write(to: url, atomically: true, encoding: .utf8)
}

func printResult(_ result: TestResult) {
    print("")
    print("┌─ \(result.name)")
    for check in result.checks {
        print("│  \(check.message)")
    }
    if !result.contentTail.isEmpty {
        print("│")
        print("│  Last 200 chars: ...\(result.contentTail) [END]")
    }
    print("│")
    print("└─ \(result.verdict.rawValue)")
}

// MARK: - Run all tests for one endpoint

func runSuite(endpoint: Endpoint, apiKey: String, outputPrefix: String) async -> [TestResult] {
    var results: [TestResult] = []
    for testCase in testCases {
        let result = await runOneTest(
            endpoint: endpoint, apiKey: apiKey,
            testCase: testCase, outputPrefix: outputPrefix
        )
        printResult(result)
        results.append(result)
    }
    return results
}

func printSummaryTable(_ results: [TestResult], title: String) {
    print("")
    print("  \(title)")
    print("  ┌─────────────────────┬───────────┬──────────────┐")
    print("  │ Test                │ Verdict   │ Notes        │")
    print("  ├─────────────────────┼───────────┼──────────────┤")
    for r in results {
        let name = r.name.padding(toLength: 19, withPad: " ", startingAt: 0)
        let verdict: String
        switch r.verdict {
        case .pass:    verdict = "✅ PASS  "
        case .partial: verdict = "⚠️  PART "
        case .fail:    verdict = "❌ FAIL  "
        }
        var notes = ""
        if r.hadStreamError { notes += "stream err " }
        let failCount = r.checks.filter { !$0.0 }.count
        if failCount > 0 && !r.hadStreamError { notes += "\(failCount) checks failed" }
        if notes.isEmpty { notes = "—" }
        let notesPadded = notes.padding(toLength: 12, withPad: " ", startingAt: 0)
        print("  │ \(name) │ \(verdict) │ \(notesPadded) │")
    }
    print("  └─────────────────────┴───────────┴──────────────┘")

    let passCount = results.filter { $0.verdict == .pass }.count
    let partialCount = results.filter { $0.verdict == .partial }.count
    let failCount = results.filter { $0.verdict == .fail }.count
    print("  \(passCount) pass, \(partialCount) partial, \(failCount) fail")
}

// MARK: - Main

let args = CommandLine.arguments
let mode: String
if args.count > 1 && ["direct", "proxy", "both"].contains(args[1]) {
    mode = args[1]
} else {
    mode = "both"
}

let outputDir = "scripts/glm5-funtimes/glm5-diagnostic-output"
try? FileManager.default.createDirectory(atPath: outputDir, withIntermediateDirectories: true)

print("")
print("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
print("  GLM-5 SSE Diagnostic")
print("  Mode: \(mode)")
print("  Tests: \(testCases.count) (\(testCases.map(\.name).joined(separator: ", ")))")
print("  Output: \(outputDir)/")
print("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

var allResults: [(title: String, results: [TestResult])] = []

// --- Direct tests ---
if mode == "direct" || mode == "both" {
    guard let nearKey = loadEnvValue("NEAR_API_KEY") else {
        print("\nERROR: NEAR_API_KEY not set")
        exit(1)
    }

    print("")
    print("╔══════════════════════════════════════════════════════════════════════")
    print("║  \(directEndpoint.name)")
    print("║  \(directEndpoint.url) → \(directEndpoint.model)")
    print("╚══════════════════════════════════════════════════════════════════════")

    let results = await runSuite(
        endpoint: directEndpoint, apiKey: nearKey,
        outputPrefix: "\(outputDir)/direct"
    )
    allResults.append((directEndpoint.name, results))
}

// --- Proxy tests ---
if mode == "proxy" || mode == "both" {
    let proxyKey = loadEnvValue("PROXY_TEST_TOKEN") ?? makeTestJWT()

    print("")
    print("╔══════════════════════════════════════════════════════════════════════")
    print("║  \(proxyEndpoint.name)")
    print("║  \(proxyEndpoint.url) → \(proxyEndpoint.model)")
    print("║  ⚠️  Requires local proxy running (make run / make run-dev)")
    print("╚══════════════════════════════════════════════════════════════════════")

    let results = await runSuite(
        endpoint: proxyEndpoint, apiKey: proxyKey,
        outputPrefix: "\(outputDir)/proxy"
    )
    allResults.append((proxyEndpoint.name, results))
}

// --- Summary ---
print("")
print("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
for (title, results) in allResults {
    printSummaryTable(results, title: title)
}
print("")
print("  Raw SSE: \(outputDir)/")
print("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
print("")

let anyFail = allResults.flatMap(\.results).contains { $0.verdict == .fail }
exit(anyFail ? 1 : 0)
