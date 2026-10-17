package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/fanice1998/kawa/internal/agent"
	"github.com/fanice1998/kawa/internal/channel"
	"github.com/fanice1998/kawa/internal/config"
	"github.com/fanice1998/kawa/internal/evolution"
	"github.com/fanice1998/kawa/internal/llm"
	"github.com/fanice1998/kawa/internal/logger"
	"github.com/fanice1998/kawa/internal/sandbox"
	"github.com/fanice1998/kawa/internal/skills"
	"github.com/fanice1998/kawa/internal/tools"
)

func main() {
	configPath := flag.String("config", "config.toml", "Path to config.toml file")
	enableCLI := flag.Bool("cli", false, "Enable interactive terminal CLI channel")
	flag.Parse()

	cfgFile := *configPath
	if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
		if _, exErr := os.Stat("config.toml.example"); exErr == nil {
			cfgFile = "config.toml.example"
		}
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config (%s): %v\n", cfgFile, err)
		os.Exit(1)
	}

	logDir := cfg.Log.Dir
	if logDir == "" {
		logDir = "./logs"
	}
	log, err := logger.Init(cfg.Log.Level, logDir, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}

	log.Info().
		Str("config", cfgFile).
		Str("default_provider", cfg.Agent.DefaultProvider).
		Bool("seatbelt_sandbox", cfg.Sandbox.UseMacOSSeatbelt).
		Msg("Initializing Kawa AI Agent")

	// 1. Initialize Go 1.24+ os.Root File System Jails
	backupDir := filepath.Join(cfg.Sandbox.StateDir, "backups", "workspace")
	workspaceJail, err := sandbox.NewFSJail(cfg.Agent.WorkspaceDir, backupDir, false)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize workspace FSJail")
	}

	// Source directory jail is strictly READ-ONLY during normal tool calls!
	sourceReadOnlyJail, err := sandbox.NewFSJail(cfg.Agent.SourceDir, "", true)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize read-only source FSJail")
	}

	// 2. Initialize macOS Seatbelt (`sandbox-exec`) Runner
	seatbeltRunner, err := sandbox.NewSeatbeltRunner(
		cfg.Sandbox.UseMacOSSeatbelt,
		cfg.Sandbox.StateDir,
		time.Duration(cfg.Sandbox.TestTimeoutSec)*time.Second,
	)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize Seatbelt sandbox runner")
	}

	// 3. Initialize Skills Manager
	skillsBackupDir := filepath.Join(cfg.Sandbox.StateDir, "backups", "skills")
	skillMgr, err := skills.NewManager(cfg.Agent.SkillsDir, skillsBackupDir)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize skills manager")
	}

	// 4. Initialize Self-Evolution Engine (Git Worktree + Seatbelt + Human Approval)
	evolveEngine, err := evolution.NewEngine(
		cfg.Agent.SourceDir,
		cfg.Sandbox.StateDir,
		seatbeltRunner,
		func(p *evolution.Proposal) {
			log.Info().
				Str("proposal_id", p.ID).
				Str("title", p.Title).
				Msg("Self-evolution applied; rebuilding Kawa binary in sandbox")

			binPath := filepath.Join(cfg.Agent.SourceDir, "bin", "kawa")
			_ = os.MkdirAll(filepath.Dir(binPath), 0o755)
			if _, statErr := os.Stat(binPath); statErr == nil {
				_ = os.Rename(binPath, binPath+".bak")
			}
			res, buildErr := seatbeltRunner.RunGo(context.Background(), cfg.Agent.SourceDir, "build", "-o", binPath, "./cmd/kawa")
			if buildErr != nil {
				log.Error().Err(buildErr).Str("stderr", res.Stderr).Msg("Rebuild after approval failed; restoring backup binary")
				if _, bakErr := os.Stat(binPath + ".bak"); bakErr == nil {
					_ = os.Rename(binPath+".bak", binPath)
				}
				return
			}
			log.Info().Str("binary", binPath).Msg("Successfully rebuilt updated Kawa binary")
		},
	)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize self-evolution engine")
	}

	// 5. Register Safe Tools (NO arbitrary bash/rm tools exist)
	toolRegistry := tools.NewRegistry()
	toolRegistry.Register(tools.NewReadFileTool(workspaceJail, sourceReadOnlyJail))
	toolRegistry.Register(tools.NewWriteFileTool(workspaceJail))
	toolRegistry.Register(tools.NewListDirTool(workspaceJail, sourceReadOnlyJail))
	toolRegistry.Register(tools.NewWebSearchTool(cfg.Tools.Web))
	toolRegistry.Register(tools.NewWebFetchTool(cfg.Tools.Web))
	toolRegistry.Register(tools.NewSelfTestTool(cfg.Agent.SourceDir, seatbeltRunner))
	toolRegistry.Register(tools.NewListSkillsTool(skillMgr))
	toolRegistry.Register(tools.NewReadSkillTool(skillMgr))
	toolRegistry.Register(tools.NewSaveSkillTool(skillMgr))
	toolRegistry.Register(evolution.NewProposeEvolutionTool(evolveEngine, nil))

	// 6. Initialize Multi-Provider LLM Registry
	llmRegistry := llm.NewRegistry(cfg.LLM, cfg.Agent.DefaultProvider)

	// 7. Initialize Extensible Communication Channels
	router := channel.NewRouter()
	activeChannels := 0
	if cfg.Channels.Telegram.Enabled {
		router.Register(channel.NewTelegramChannel(cfg.Channels.Telegram))
		activeChannels++
	}
	if cfg.Channels.IMessage.Enabled {
		router.Register(channel.NewIMessageChannel(cfg.Channels.IMessage))
		activeChannels++
	}
	if *enableCLI || activeChannels == 0 {
		router.Register(channel.NewCLIChannel())
	}

	// 8. Start Agent Loop with Graceful Shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	kawaAgent := agent.New(cfg, llmRegistry, toolRegistry, skillMgr, evolveEngine, router)
	if err := kawaAgent.Run(ctx); err != nil {
		log.Fatal().Err(err).Msg("Kawa Agent terminated with error")
	}
}
