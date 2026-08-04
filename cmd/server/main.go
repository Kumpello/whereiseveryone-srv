package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"whereiseveryone/internal/config"

	"github.com/sirupsen/logrus"
	echoSwagger "github.com/swaggo/echo-swagger"
	"golang.org/x/crypto/bcrypt"

	"whereiseveryone/internal/mongo"
	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi"
	authMux "whereiseveryone/internal/webapi/auth"
	meMux "whereiseveryone/internal/webapi/me"
	"whereiseveryone/pkg/crypto"
	"whereiseveryone/pkg/env"
	"whereiseveryone/pkg/jwt"
	"whereiseveryone/pkg/logger"
	"whereiseveryone/pkg/timer"

	"github.com/go-playground/validator"

	_ "whereiseveryone/docs"

	_ "github.com/swaggo/echo-swagger" // echo-swagger middleware
)

const (
	defaultBcryptCost     = crypto.DefaultPasswordHashCost
	indexCreationTimeout  = 30 * time.Second
	serverReadTimeout     = 10 * time.Second
	serverWriteTimeout    = 30 * time.Second
	serverIdleTimeout     = 120 * time.Second
)

// @title WhereIsEveryone
// @version 1.0
// @description This is a sample server for WhereIsEveryone

// @license.name Apache 2.0
// @license.url http://www.apache.org/licenses/LICENSE-2.0.html

// @securityDefinitions.apikey Bearer
// @in header
// @name Authorization

// @BasePath /api

func main() {
	// Flags
	configPathFlag := flag.String("config", "./.env/local.json", "config path")
	flag.Parse()

	// global dependencies
	log := logger.NewLogger()

	log.Infof("using config path: %s", *configPathFlag)

	envHandler, err := env.NewHandler(*configPathFlag)
	appCtx := context.Background()
	utcTimer := timer.NewUTCTimer()
	if err != nil {
		log.Fatalf("loading config: %s", err.Error())
	}

	isDebug := envHandler.MustEnv(config.ConfDebug) == "true"
	if isDebug {
		log.SetLevel(logrus.DebugLevel)
		log.SetReportCaller(true)
	} else {
		log.SetLevel(logrus.InfoLevel)
	}

	// Mongo
	mongoCollections, err := mongo.GetMongo(appCtx, envHandler)
	if err != nil {
		log.Fatalf("init mongo: %s", err.Error())
	}
	defer mongoCollections.Disconnect(appCtx)
	usersAdapter := users.NewMongoAdapter(mongoCollections.Users, mongoCollections.PendingFriendRequests, utcTimer, log)
	indexCtx, cancelIndexCreation := context.WithTimeout(appCtx, indexCreationTimeout)
	if err := usersAdapter.EnsureIndexes(indexCtx); err != nil {
		cancelIndexCreation()
		log.Fatalf("ensure mongo indexes: %s", err.Error())
	}
	cancelIndexCreation()

	// Echo
	jwtSecret := envHandler.MustEnv(config.ConfJwtSecret)
	// TODO: Get VALIDITY from config
	jwtInstance := jwt.NewJWT(utcTimer, []byte(jwtSecret), time.Duration(15)*time.Minute, time.Duration(720)*time.Hour)

	authRouter := authMux.NewMux(usersAdapter, utcTimer, jwtInstance)
	authRouter.SetPasswordHashCost(bcryptCostFromEnv(envHandler, log))
	meRouter := meMux.NewMux(usersAdapter, utcTimer)

	validate := validator.New()
	e := webapi.NewEcho(
		"/api",
		validate,
		jwtInstance,
		webapi.EchoRouters{
			Swagger:    echoSwagger.WrapHandler,
			AuthRouter: authRouter,
			MeRouter:   meRouter,
		},
		log,
		isDebug)

	// Start server
	port := envHandler.MustEnv(config.ConfAppPort)
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%s", port),
		ReadTimeout:  serverReadTimeout,
		WriteTimeout: serverWriteTimeout,
		IdleTimeout:  serverIdleTimeout,
	}
	log.Fatal(e.StartServer(srv))
}

func intFromEnv(envHandler env.Handler, key env.Key, fallback int, log logger.Logger) int {
	raw := envHandler.Env(key, strconv.Itoa(fallback))
	value, err := strconv.Atoi(raw)
	if err != nil {
		log.Warnf("invalid integer config %s=%q, using %d", key, raw, fallback)
		return fallback
	}

	return value
}

func bcryptCostFromEnv(envHandler env.Handler, log logger.Logger) int {
	cost := intFromEnv(envHandler, config.ConfBcryptCost, defaultBcryptCost, log)
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		log.Warnf("bcrypt cost %d is outside [%d,%d], using %d", cost, bcrypt.MinCost, bcrypt.MaxCost, defaultBcryptCost)
		return defaultBcryptCost
	}

	return cost
}
