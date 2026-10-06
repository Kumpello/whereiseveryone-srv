package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"time"
	"whereiseveryone/internal/config"

	"github.com/sirupsen/logrus"
	echoSwagger "github.com/swaggo/echo-swagger/v2"

	"whereiseveryone/internal/mongo"
	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi"
	authMux "whereiseveryone/internal/webapi/auth"
	meMux "whereiseveryone/internal/webapi/me"
	"whereiseveryone/pkg/env"
	"whereiseveryone/pkg/jwt"
	"whereiseveryone/pkg/logger"
	"whereiseveryone/pkg/timer"

	"github.com/go-playground/validator/v10"

	_ "whereiseveryone/docs"
)

const (
	indexCreationTimeout = 30 * time.Second
	serverReadTimeout    = 10 * time.Second
	serverWriteTimeout   = 30 * time.Second
	serverIdleTimeout    = 120 * time.Second
)

// @title WhereIsEveryone
// @version 1.0
// @description Bodies: max 16 KiB (413), application/json (415). Auth/me busy: 503 with Retry-After: 1.

// @license.name Apache 2.0
// @license.url http://www.apache.org/licenses/LICENSE-2.0.html

// @securityDefinitions.apikey Bearer
// @in header
// @name Authorization

// @BasePath /

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

	jwtSecret, err := config.JWTSecret(envHandler)
	if err != nil {
		log.Fatalf("invalid configuration: %s", err)
	}
	requestLimits, err := config.DatabaseRequestLimitsFromEnv(envHandler)
	if err != nil {
		log.Fatalf("invalid database request limits: %s", err)
	}
	passwordLimits, err := config.PasswordWorkLimitsFromEnv(envHandler)
	if err != nil {
		log.Fatalf("invalid password work limits: %s", err)
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
	jwtInstance := jwt.NewJWT(utcTimer, []byte(jwtSecret), time.Duration(15)*time.Minute, time.Duration(720)*time.Hour)

	authRouter := authMux.NewMux(usersAdapter, utcTimer, jwtInstance)
	if err := authRouter.SetPasswordWorkLimits(passwordLimits); err != nil {
		log.Fatalf("configure authentication: %s", err)
	}
	meRouter := meMux.NewMux(usersAdapter, utcTimer)

	validate := validator.New()
	e := webapi.NewEcho(
		"",
		validate,
		jwtInstance,
		usersAdapter,
		webapi.EchoRouters{
			Swagger:    echoSwagger.WrapHandler,
			AuthRouter: authRouter,
			MeRouter:   meRouter,
		},
		log,
		isDebug,
		requestLimits)

	// Start server
	port := envHandler.MustEnv(config.ConfAppPort)
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%s", port),
		Handler:      e,
		ReadTimeout:  serverReadTimeout,
		WriteTimeout: serverWriteTimeout,
		IdleTimeout:  serverIdleTimeout,
	}
	log.Fatal(srv.ListenAndServe())
}
