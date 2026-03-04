import Foundation
import OpenAI

// MARK: - Configuration

struct ModelDef {
    let name: String
    let host: String
    let apiKeyEnv: String
    let upstreamModel: String
    let basePath: String

    init(name: String, host: String, apiKeyEnv: String, upstreamModel: String, basePath: String = "/v1") {
        self.name = name
        self.host = host
        self.apiKeyEnv = apiKeyEnv
        self.upstreamModel = upstreamModel
        self.basePath = basePath
    }
}

let allModels: [ModelDef] = [
    // Tinfoil (TEE)
    ModelDef(name: "deepseek-r1",    host: "inference.tinfoil.sh", apiKeyEnv: "TINFOIL_API_KEY", upstreamModel: "deepseek-r1-0528"),

    // NEAR AI: self-hosted models
    ModelDef(name: "glm-5",          host: "cloud-api.near.ai",    apiKeyEnv: "NEAR_API_KEY",     upstreamModel: "zai-org/GLM-5-FP8"),
    ModelDef(name: "qwen3-30b",      host: "cloud-api.near.ai",    apiKeyEnv: "NEAR_API_KEY",     upstreamModel: "Qwen/Qwen3-30B-A3B-Instruct-2507"),
    ModelDef(name: "deepseek-v3.1",  host: "cloud-api.near.ai",    apiKeyEnv: "NEAR_API_KEY",     upstreamModel: "deepseek-ai/DeepSeek-V3.1"),
    ModelDef(name: "gpt-oss-120b",   host: "cloud-api.near.ai",    apiKeyEnv: "NEAR_API_KEY",     upstreamModel: "openai/gpt-oss-120b"),

    // NEAR AI: passthrough to external providers (tests NEAR AI proxy layer)
    ModelDef(name: "near-sonnet",    host: "cloud-api.near.ai",    apiKeyEnv: "NEAR_API_KEY",     upstreamModel: "anthropic/claude-sonnet-4-5"),
    ModelDef(name: "near-gpt5",      host: "cloud-api.near.ai",    apiKeyEnv: "NEAR_API_KEY",     upstreamModel: "openai/gpt-5.2"),

    // OpenRouter: reference models (proves our code works with known-good providers)
    ModelDef(name: "or-opus",        host: "openrouter.ai", apiKeyEnv: "OPENROUTER_API_KEY", upstreamModel: "anthropic/claude-opus-4", basePath: "/api/v1"),
    ModelDef(name: "or-gpt5",        host: "openrouter.ai", apiKeyEnv: "OPENROUTER_API_KEY", upstreamModel: "openai/gpt-5.2",          basePath: "/api/v1"),
    ModelDef(name: "or-glm5",        host: "openrouter.ai", apiKeyEnv: "OPENROUTER_API_KEY", upstreamModel: "thudm/glm-5-0185",        basePath: "/api/v1"),
]

let prompt = """
Write a creative story of approximately 500 words about a robot discovering music for the first time. \
The story MUST end with the exact phrase THE END on its own line as the very last thing you write. \
Do not add anything after THE END.
"""

let sentinel = "THE END"
let minContentLength = 400
let maxAvgChunkChars = 200
let minChunkCount = 10

// MARK: - Load .env

func loadEnv() {
    // Walk up from CWD looking for .env
    var dir = URL(fileURLWithPath: FileManager.default.currentDirectoryPath)
    for _ in 0..<5 {
        let envFile = dir.appendingPathComponent(".env")
        if FileManager.default.fileExists(atPath: envFile.path),
           let contents = try? String(contentsOf: envFile, encoding: .utf8) {
            for line in contents.components(separatedBy: .newlines) {
                let trimmed = line.trimmingCharacters(in: .whitespaces)
                guard !trimmed.isEmpty, !trimmed.hasPrefix("#") else { continue }
                if let eqIdx = trimmed.firstIndex(of: "=") {
                    let key = String(trimmed[trimmed.startIndex..<eqIdx])
                    let value = String(trimmed[trimmed.index(after: eqIdx)...])
                    // Skip JSON values
                    guard !value.hasPrefix("{") else { continue }
                    setenv(key, value, 0) // don't overwrite existing
                }
            }
            return
        }
        dir = dir.deletingLastPathComponent()
    }
}

// MARK: - Test runner

struct TestResult {
    let model: String
    let passed: Bool
    let checks: [(passed: Bool, message: String)]
}

func testModel(_ def: ModelDef) async -> TestResult {
    var checks: [(Bool, String)] = []

    func pass(_ msg: String) { checks.append((true, "✓ \(msg)")) }
    func fail(_ msg: String) { checks.append((false, "✗ \(msg)")) }
    func info(_ msg: String) { checks.append((true, "ℹ \(msg)")) }

    guard let apiKey = ProcessInfo.processInfo.environment[def.apiKeyEnv], !apiKey.isEmpty else {
        fail("\(def.apiKeyEnv) not set")
        return TestResult(model: def.name, passed: false, checks: checks)
    }

    info("Provider: \(def.host) → \(def.upstreamModel)")

    let config = OpenAI.Configuration(
        token: apiKey,
        host: def.host,
        port: 443,
        scheme: "https",
        basePath: def.basePath,
        timeoutInterval: 300
    )
    let client = OpenAI(configuration: config)

    let query = ChatQuery(
        messages: [.user(.init(content: .string(prompt)))],
        model: def.upstreamModel,
        stream: true
    )

    // Stream and collect chunks with timing
    var content = ""
    var reasoning = ""
    var contentChunks: [String] = []
    var maxContentChunk = 0
    var reasoningChunkCount = 0
    var finishReason: String?
    var chunkTimestamps: [CFAbsoluteTime] = []
    var gotError: String?

    let startTime = CFAbsoluteTimeGetCurrent()
    var firstChunkTime: CFAbsoluteTime?

    do {
        let stream: AsyncThrowingStream<ChatStreamResult, Error> = client.chatsStream(query: query)
        for try await result in stream {
            let now = CFAbsoluteTimeGetCurrent()
            if firstChunkTime == nil { firstChunkTime = now }

            guard let choice = result.choices.first else { continue }

            if let c = choice.delta.content, !c.isEmpty {
                content += c
                contentChunks.append(c)
                chunkTimestamps.append(now)
                if c.count > maxContentChunk { maxContentChunk = c.count }
            }

            if let r = choice.delta.reasoning, !r.isEmpty {
                reasoning += r
                reasoningChunkCount += 1
            }

            if let fr = choice.finishReason {
                finishReason = fr.rawValue
            }
        }
    } catch {
        gotError = error.localizedDescription
    }

    let totalTime = CFAbsoluteTimeGetCurrent() - startTime
    let ttfb = firstChunkTime.map { $0 - startTime } ?? totalTime

    // Check 1: No errors
    if let err = gotError {
        fail("Stream error: \(err)")
        return TestResult(model: def.name, passed: false, checks: checks)
    }
    pass("Stream completed (TTFB: \(String(format: "%.1f", ttfb))s, Total: \(String(format: "%.1f", totalTime))s)")

    if reasoningChunkCount > 0 {
        info("Reasoning: \(reasoningChunkCount) chunks, \(reasoning.count) chars")
    }

    // Check 2: Has content
    if !contentChunks.isEmpty {
        pass("Content produced: \(contentChunks.count) chunks, \(content.count) chars")
    } else {
        fail("NO content — only reasoning (\(reasoningChunkCount) chunks, \(reasoning.count) chars)")
        if !reasoning.isEmpty {
            let tail = String(reasoning.suffix(150))
            info("Last 150 chars of reasoning: ...\(tail)")
        }
    }

    // Check 3: finish_reason
    if finishReason == "stop" {
        pass("finish_reason: stop")
    } else if finishReason == "length" {
        fail("finish_reason: length — hit token limit")
    } else if let fr = finishReason {
        info("finish_reason: \(fr)")
    } else {
        fail("No finish_reason — stream ended abnormally")
    }

    // Content-specific checks (only if content was produced)
    if !contentChunks.isEmpty {
        // Check 4: Enough chunks
        if contentChunks.count >= minChunkCount {
            pass("Chunk count: \(contentChunks.count) (min: \(minChunkCount))")
        } else {
            fail("Only \(contentChunks.count) chunks (min: \(minChunkCount))")
        }

        // Check 5: Chunks are small
        let avgChunk = content.count / contentChunks.count
        if avgChunk <= maxAvgChunkChars {
            pass("Avg chunk: \(avgChunk) chars (max: \(maxAvgChunkChars))")
        } else {
            fail("Avg chunk: \(avgChunk) chars — chunky streaming")
        }
        info("Largest chunk: \(maxContentChunk) chars")

        // Check 6: Enough content
        if content.count >= minContentLength {
            pass("Content length: \(content.count) chars (min: \(minContentLength))")
        } else {
            fail("Content: \(content.count) chars (min: \(minContentLength)) — too short")
        }

        // Check 7: Sentinel phrase
        let tail = String(content.suffix(200))
        if content.lowercased().contains(sentinel.lowercased()) {
            pass("Sentinel \"\(sentinel)\" found")
        } else {
            fail("Sentinel \"\(sentinel)\" NOT found — possible cutoff")
        }
        info("Last 200 chars: ...\(tail) [STREAM ENDED]")

        // Check 8: Streaming timing (are chunks arriving over time or all at once?)
        if chunkTimestamps.count >= 2 {
            let first = chunkTimestamps.first!
            let last = chunkTimestamps.last!
            let span = last - first
            if span > 1.0 {
                pass("Chunks spread over \(String(format: "%.1f", span))s (real streaming)")
            } else {
                fail("All \(chunkTimestamps.count) chunks arrived within \(String(format: "%.2f", span))s — batched, not streaming")
            }
        }
    }

    let allPassed = checks.allSatisfy { $0.0 }
    return TestResult(model: def.name, passed: allPassed, checks: checks)
}

// MARK: - Main

loadEnv()

// Filter models if MODELS env var is set
let requestedModels: [String]?
if let modelsEnv = ProcessInfo.processInfo.environment["MODELS"], !modelsEnv.isEmpty {
    requestedModels = modelsEnv.components(separatedBy: " ")
} else {
    requestedModels = nil
}

let modelsToTest = allModels.filter { def in
    requestedModels == nil || requestedModels!.contains(def.name)
}

print("")
print("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
print("  Streaming Smoke Test (Swift/OpenAI SDK)")
print("  Models: \(modelsToTest.map(\.name).joined(separator: ", "))")
print("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

// Run all tests in parallel
let results: [TestResult] = await withTaskGroup(of: TestResult.self) { group in
    for def in modelsToTest {
        group.addTask { await testModel(def) }
    }
    var collected: [TestResult] = []
    for await result in group {
        collected.append(result)
    }
    return collected
}

// Print results in original model order
var totalPass = 0
var totalFail = 0
var failedModels: [String] = []

let orderedNames = modelsToTest.map(\.name)
let sortedResults = results.sorted { a, b in
    (orderedNames.firstIndex(of: a.model) ?? Int.max) < (orderedNames.firstIndex(of: b.model) ?? Int.max)
}

for result in sortedResults {
    print("")
    print("┌─ Testing: \(result.model)")
    print("│")

    for check in result.checks {
        print("  \(check.message)")
    }

    print("│")
    if result.passed {
        print("└─ ✅ PASS")
        totalPass += 1
    } else {
        print("└─ ❌ FAIL")
        totalFail += 1
        failedModels.append(result.model)
    }
}

print("")
print("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
print("  Results: \(totalPass) passed, \(totalFail) failed")
if !failedModels.isEmpty {
    print("  Failed:  \(failedModels.joined(separator: ", "))")
}
print("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
print("")

exit(totalFail > 0 ? 1 : 0)
