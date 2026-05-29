package main

import (
	"context"
	"flag"
	"fmt"
	"time"
	"whereiseveryone/internal/config"

	"github.com/sirupsen/logrus"
	echoSwagger "github.com/swaggo/echo-swagger"

	"whereiseveryone/internal/mongo"
	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi"
	authMux "whereiseveryone/internal/webapi/auth"
	meMux "whereiseveryone/internal/webapi/me"
	"whereiseveryone/pkg/env"
	"whereiseveryone/pkg/jwt"
	"whereiseveryone/pkg/logger"
	"whereiseveryone/pkg/timer"

	"github.com/go-playground/validator"

	_ "whereiseveryone/docs"

	_ "github.com/swaggo/echo-swagger" // echo-swagger middleware
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
	// TODO: Get LOGGING_LEVEL from config
	log.SetLevel(logrus.DebugLevel)

	log.Infof("using config path: %s", *configPathFlag)

	envHandler, err := env.NewHandler(*configPathFlag)
	appCtx := context.Background()
	utcTimer := timer.NewUTCTimer()
	if err != nil {
		log.Fatalf("loading config: %s", err.Error())
	}

	// Mongo
	mongoCollections, err := mongo.GetMongo(appCtx, envHandler)
	if err != nil {
		log.Fatalf("init mongo: %s", err.Error())
		panic(err)
	}
	defer mongoCollections.Disconnect(appCtx)
	usersAdapter := users.NewMongoAdapter(mongoCollections.Users, mongoCollections.PendingFriendRequests, utcTimer, log)

	// Echo
	jwtSecret := envHandler.MustEnv(config.ConfJwtSecret)
	// TODO: Get VALIDITY from config
	jwtInstance := jwt.NewJWT(utcTimer, []byte(jwtSecret), time.Duration(15)*time.Minute, time.Duration(720)*time.Hour)

	authRouter := authMux.NewMux(usersAdapter, utcTimer, jwtInstance)
	meRouter := meMux.NewMux(usersAdapter, utcTimer)

	isDebug := envHandler.MustEnv(config.ConfDebug)
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
		isDebug == "true")

	// Start server
	port := envHandler.MustEnv(config.ConfAppPort)
	log.Fatal(e.Start(fmt.Sprintf(":%s", port)))
}
