package commands

import (
	"context"
	"github.com/spf13/cobra"
	"whereiseveryone/internal/config"
	"whereiseveryone/internal/mongo"
	"whereiseveryone/pkg/env"
	"whereiseveryone/pkg/logger"
	"whereiseveryone/pkg/timer"
)

type commandApp struct {
	*cobra.Command

	logger logger.Logger
	timer  timer.Timer
}

func NewCommandApp(
	lg logger.Logger,
	tm timer.Timer,
) *commandApp {
	rootCmd := &cobra.Command{
		Use:   "cli",
		Short: "Some useful commands for where is everyone server app",
		Long: `
			The app allows to execute some useful commands,
  			required to manage the app
			Can be easily used with k8s-jobs or any other scheduler.
			`,
	}

	rootCmd.PersistentFlags().String("config", "./.env/local.json", "path to config file")

	app := &commandApp{
		Command: rootCmd,
		logger:  lg,
		timer:   tm,
	}

	dummyCmd := &cobra.Command{
		Use:   "dummy",
		Short: "Just print debug info on the screen",
		Run: func(cmd *cobra.Command, args []string) {
			app.logger.Info("You are a dummy!")
		},
	}

	mongoIndexes := &cobra.Command{
		Use:   "mongoIndexes",
		Short: "sets mongo indexes (must be run after each index change)",
		Run: func(cmd *cobra.Command, args []string) {
			app.mongoIndexes(cmd.Context())
		},
	}

	rootCmd.AddCommand(dummyCmd)
	rootCmd.AddCommand(mongoIndexes)
	initConfig := &cobra.Command{
		Use:   "initConfig",
		Short: "Create a private configuration with a fresh JWT secret",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			templatePath, err := cmd.Flags().GetString("template")
			if err != nil {
				return err
			}
			outputPath, err := cmd.Flags().GetString("config")
			if err != nil {
				return err
			}
			return config.CreateFromTemplate(templatePath, outputPath)
		},
	}
	initConfig.Flags().String("template", "./.env/local.example.json", "configuration template path")
	rootCmd.AddCommand(initConfig)

	return app
}

func (c *commandApp) mustGetEnvHandler() env.Handler {
	config := c.Flag("config")
	configPath := config.Value.String()
	envHandler, err := env.NewHandler(configPath)
	if err != nil {
		c.logger.Fatalf("getting env handler: %s", err.Error())
	}
	return envHandler
}

func (c *commandApp) mustGetMongoCollections(ctx context.Context, envHandler env.Handler) *mongo.Collections {
	mongoCollections, err := mongo.GetMongo(ctx, envHandler)
	if err != nil {
		c.logger.Fatalf("connecting with mongo: %s", err.Error())
	}
	return mongoCollections
}
