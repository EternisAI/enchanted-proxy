package fai

import "fmt"

// TokenConfig holds per-token payment configuration.
type TokenConfig struct {
	CoingeckoID string `json:"coingecko_id"`
	Decimals    int    `json:"decimals"`
}

// GetTokenConfigByChainID returns token configurations for the given chain.
func GetTokenConfigByChainID(chainID uint64) (map[string]TokenConfig, error) {
	configs := map[uint64]map[string]TokenConfig{
		// Base Sepolia (testnet)
		84532: {
			"0xC353B6E76e7254Ae14EfDF856E5997AA4Aef6E07": {
				CoingeckoID: "freysa-ai",
				Decimals:    18,
			},
		},
		// Base Mainnet
		8453: {
			"0xb33Ff54b9F7242EF1593d2C9Bcd8f9df46c77935": {
				CoingeckoID: "freysa-ai",
				Decimals:    18,
			},
		},
	}

	tokenConfig, exists := configs[chainID]
	if !exists {
		return nil, fmt.Errorf("no token configuration found for chain ID %d", chainID)
	}

	return tokenConfig, nil
}
