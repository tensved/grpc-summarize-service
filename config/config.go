package config

import (
	"os"
	"strconv"
)

// Config contains all service configuration parameters
type Config struct {
	GRPCPort   string
	HealthPort string

	AIServiceURL   string
	AIModel        string
	AIBackupModel  string // Fallback model if primary model returns 503
	AISystemPrompt string
	MaxMessages    int32
	AIToken        string

	LogLevel string
}

// Global contains global configuration
var Global *Config

// Init initializes configuration from environment variables
func Init() {
	Global = &Config{
		GRPCPort:       getEnv("GRPC_PORT", "8088"),
		HealthPort:     getEnv("HEALTH_PORT", "8089"),
		AIServiceURL:   getEnv("AI_SERVICE_URL", "https://..."),
		AIModel:        getEnv("AI_MODEL", "CHATGPT"),
		AIBackupModel:  getEnv("AI_BACKUP_MODEL", ""), // Optional fallback model for 503 errors
		AISystemPrompt: getEnv("AI_SYSTEM_PROMPT", "You are a helpful assistant that summarizes conversations. Provide a concise summary of the key points discussed. ⚠️ Important: Your response must be written **exclusively in English**, without using any words or phrases from other languages. If the input is not in English, translate it and then continue the answer in English only."),
		MaxMessages:    getEnvInt32("MAX_MESSAGES_PER_REQUEST", 50),
		AIToken:        getEnv("AI_TOKEN", ""),
		LogLevel:       getEnv("LOG_LEVEL", "info"),
	}
}

// getEnv gets environment variable or returns default value
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvInt32 gets environment variable as int32 or returns default value
func getEnvInt32(key string, defaultValue int32) int32 {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return int32(intValue)
		}
	}
	return defaultValue
}
