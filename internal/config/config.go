package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// Config represents the root configuration loaded via spf13/viper.
type Config struct {
	Agent    AgentConfig    `mapstructure:"agent"`
	Log      LogConfig      `mapstructure:"log"`
	Sandbox  SandboxConfig  `mapstructure:"sandbox"`
	LLM      LLMConfig      `mapstructure:"llm"`
	Channels ChannelsConfig `mapstructure:"channels"`
	Tools    ToolsConfig    `mapstructure:"tools"`
}

type AgentConfig struct {
	Name            string `mapstructure:"name"`
	WorkspaceDir    string `mapstructure:"workspace_dir"`
	SourceDir       string `mapstructure:"source_dir"`
	SkillsDir       string `mapstructure:"skills_dir"`
	MaxIterations   int    `mapstructure:"max_iterations"`
	DefaultProvider string `mapstructure:"default_provider"`
	SystemPrompt    string `mapstructure:"system_prompt"`
}

type LogConfig struct {
	Level    string `mapstructure:"level"`
	Dir      string `mapstructure:"dir"`
	FilePath string `mapstructure:"file_path"`
}

type SandboxConfig struct {
	Enabled              bool   `mapstructure:"enabled"`
	UseMacOSSeatbelt     bool   `mapstructure:"use_macos_seatbelt"`
	TestTimeoutSec       int    `mapstructure:"test_timeout_sec"`
	RequireHumanApproval bool   `mapstructure:"require_human_approval"`
	StateDir             string `mapstructure:"state_dir"`
}

type LLMConfig struct {
	Ollama      OllamaConfig      `mapstructure:"ollama"`
	XAIGrok     APIKeyModelConfig `mapstructure:"xai_grok"`
	GrokOAuth   OAuthModelConfig  `mapstructure:"grok_oauth"`
	Claude      ClaudeConfig      `mapstructure:"claude"`
	Gemini      APIKeyModelConfig `mapstructure:"gemini"`
	GeminiOAuth OAuthModelConfig  `mapstructure:"gemini_oauth"`
}

type OllamaConfig struct {
	BaseURL    string `mapstructure:"base_url"`
	Model      string `mapstructure:"model"`
	TimeoutSec int    `mapstructure:"timeout_sec"`
}

type APIKeyModelConfig struct {
	BaseURL    string `mapstructure:"base_url"`
	APIKey     string `mapstructure:"api_key"`
	Model      string `mapstructure:"model"`
	TimeoutSec int    `mapstructure:"timeout_sec"`
}

type OAuthModelConfig struct {
	BaseURL      string `mapstructure:"base_url"`
	Model        string `mapstructure:"model"`
	AccessToken  string `mapstructure:"access_token"`
	RefreshToken string `mapstructure:"refresh_token"`
	TokenURL     string `mapstructure:"token_url"`
	ClientID     string `mapstructure:"client_id"`
	ClientSecret string `mapstructure:"client_secret"`
	ProjectID    string `mapstructure:"project_id"`
	ExpiresAt    string `mapstructure:"expires_at"`
	TimeoutSec   int    `mapstructure:"timeout_sec"`
}

type ClaudeConfig struct {
	BaseURL          string `mapstructure:"base_url"`
	APIKey           string `mapstructure:"api_key"`
	Model            string `mapstructure:"model"`
	MaxTokens        int    `mapstructure:"max_tokens"`
	AnthropicVersion string `mapstructure:"anthropic_version"`
	TimeoutSec       int    `mapstructure:"timeout_sec"`
}

type ChannelsConfig struct {
	Telegram TelegramChannelConfig `mapstructure:"telegram"`
	IMessage IMessageChannelConfig `mapstructure:"imessage"`
}

type TelegramChannelConfig struct {
	Enabled        bool    `mapstructure:"enabled"`
	BotToken       string  `mapstructure:"bot_token"`
	AllowedUserIDs []int64 `mapstructure:"allowed_user_ids"`
	PollTimeoutSec int     `mapstructure:"poll_timeout_sec"`
}

type IMessageChannelConfig struct {
	Enabled        bool     `mapstructure:"enabled"`
	ChatDBPath     string   `mapstructure:"chat_db_path"`
	PollIntervalMs int      `mapstructure:"poll_interval_ms"`
	AllowedHandles []string `mapstructure:"allowed_handles"`
}

type ToolsConfig struct {
	Web WebToolConfig `mapstructure:"web"`
}

type WebToolConfig struct {
	SearchProvider string `mapstructure:"search_provider"`
	BraveAPIKey    string `mapstructure:"brave_api_key"`
	UserAgent      string `mapstructure:"user_agent"`
	TimeoutSec     int    `mapstructure:"timeout_sec"`
	MaxFetchBytes  int64  `mapstructure:"max_fetch_bytes"`
}

// Load reads configuration from a TOML file using spf13/viper and applies safe defaults.
func Load(configPath string) (*Config, error) {
	v := viper.New()
	setDefaults(v)

	v.SetConfigType("toml")
	if strings.TrimSpace(configPath) != "" {
		v.SetConfigFile(configPath)
	} else {
		v.SetConfigName("config")
		v.AddConfigPath(".")
	}

	v.SetEnvPrefix("KAWA")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	if err := cfg.NormalizePaths(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("agent.name", "Kawa")
	v.SetDefault("agent.workspace_dir", "./workspace")
	v.SetDefault("agent.source_dir", ".")
	v.SetDefault("agent.skills_dir", "./skills")
	v.SetDefault("agent.max_iterations", 15)
	v.SetDefault("agent.default_provider", "ollama")

	v.SetDefault("log.level", "info")
	v.SetDefault("log.dir", "./logs")

	v.SetDefault("sandbox.enabled", true)
	v.SetDefault("sandbox.use_macos_seatbelt", true)
	v.SetDefault("sandbox.test_timeout_sec", 120)
	v.SetDefault("sandbox.require_human_approval", true)
	v.SetDefault("sandbox.state_dir", "./.kawa")

	v.SetDefault("llm.ollama.base_url", "http://127.0.0.1:11434")
	v.SetDefault("llm.ollama.model", "qwen2.5-coder:14b")
	v.SetDefault("llm.ollama.timeout_sec", 120)

	v.SetDefault("llm.xai_grok.base_url", "https://api.x.ai/v1")
	v.SetDefault("llm.xai_grok.model", "grok-2-latest")
	v.SetDefault("llm.xai_grok.timeout_sec", 120)

	v.SetDefault("llm.grok_oauth.base_url", "https://api.x.ai/v1")
	v.SetDefault("llm.grok_oauth.model", "grok-2-latest")
	v.SetDefault("llm.grok_oauth.token_url", "https://api.x.ai/v1/oauth2/token")
	v.SetDefault("llm.grok_oauth.timeout_sec", 120)

	v.SetDefault("llm.claude.base_url", "https://api.anthropic.com/v1")
	v.SetDefault("llm.claude.model", "claude-3-7-sonnet-latest")
	v.SetDefault("llm.claude.max_tokens", 8192)
	v.SetDefault("llm.claude.anthropic_version", "2023-06-01")
	v.SetDefault("llm.claude.timeout_sec", 120)

	v.SetDefault("llm.gemini.base_url", "https://generativelanguage.googleapis.com/v1beta")
	v.SetDefault("llm.gemini.model", "gemini-2.5-pro")
	v.SetDefault("llm.gemini.timeout_sec", 120)

	v.SetDefault("llm.gemini_oauth.base_url", "https://generativelanguage.googleapis.com/v1beta")
	v.SetDefault("llm.gemini_oauth.model", "gemini-2.5-pro")
	v.SetDefault("llm.gemini_oauth.token_url", "https://oauth2.googleapis.com/token")
	v.SetDefault("llm.gemini_oauth.timeout_sec", 120)

	v.SetDefault("channels.telegram.poll_timeout_sec", 30)
	v.SetDefault("channels.imessage.chat_db_path", "~/Library/Messages/chat.db")
	v.SetDefault("channels.imessage.poll_interval_ms", 2000)

	v.SetDefault("tools.web.search_provider", "duckduckgo")
	v.SetDefault("tools.web.user_agent", "KawaAgent/1.0")
	v.SetDefault("tools.web.timeout_sec", 20)
	v.SetDefault("tools.web.max_fetch_bytes", 204800)
}

// NormalizePaths expands ~ and resolves relative paths into clean absolute paths.
func (c *Config) NormalizePaths() error {
	var err error
	if c.Log.Dir == "" && c.Log.FilePath != "" {
		c.Log.Dir = "./logs"
	}
	if c.Log.Dir != "" {
		if c.Log.Dir, err = expandAndAbs(c.Log.Dir); err != nil {
			return fmt.Errorf("resolve log.dir: %w", err)
		}
	}
	if c.Agent.WorkspaceDir, err = expandAndAbs(c.Agent.WorkspaceDir); err != nil {
		return fmt.Errorf("resolve workspace_dir: %w", err)
	}
	if c.Agent.SourceDir, err = expandAndAbs(c.Agent.SourceDir); err != nil {
		return fmt.Errorf("resolve source_dir: %w", err)
	}
	if c.Agent.SkillsDir, err = expandAndAbs(c.Agent.SkillsDir); err != nil {
		return fmt.Errorf("resolve skills_dir: %w", err)
	}
	if c.Sandbox.StateDir, err = expandAndAbs(c.Sandbox.StateDir); err != nil {
		return fmt.Errorf("resolve state_dir: %w", err)
	}
	if c.Channels.IMessage.ChatDBPath, err = expandAndAbs(c.Channels.IMessage.ChatDBPath); err != nil {
		return fmt.Errorf("resolve imessage chat_db_path: %w", err)
	}
	return nil
}

func expandAndAbs(p string) (string, error) {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[2:])
	}
	return filepath.Abs(p)
}
