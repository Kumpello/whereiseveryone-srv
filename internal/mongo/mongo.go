package mongo

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"whereiseveryone/internal/config"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const timeout = time.Duration(30) * time.Second

type Indexable interface {
	EnsureIndexes(ctx context.Context) error
}

type Collections struct {
	client *mongo.Client

	Users                 *mongo.Collection
	PendingFriendRequests *mongo.Collection
}

func (c *Collections) Disconnect(ctx context.Context) error {
	return c.client.Disconnect(ctx)
}

// NewMongoWithX509Pem connects with a finite pool cap, defaulting to eight when omitted.
func NewMongoWithX509Pem(ctx context.Context, db, uri, tlsCertPath string,
	maxPoolSize ...uint64,
) (*Collections, error) {
	connStr := "mongodb+srv://" +
		uri +
		"/?authSource=%24external&authMechanism=" +
		"MONGODB-X509&retryWrites=true&w=majority&tlsCertificateKeyFile=" +
		url.QueryEscape(tlsCertPath)

	serverAPIOptions := options.ServerAPI(options.ServerAPIVersion1)
	clientOptions := options.Client().
		ApplyURI(connStr).
		SetServerAPIOptions(serverAPIOptions)

	return newMongo(ctx, db, clientOptions, maxPoolSize...)
}

// NewMongoWithPassword connects with a finite pool cap that overrides the URI's maxPoolSize.
func NewMongoWithPassword(ctx context.Context, db, uri, authDB, user, pass string,
	maxPoolSize ...uint64,
) (*Collections, error) {
	opts := options.Client().ApplyURI(uri)
	opts.SetServerSelectionTimeout(timeout)
	opts.SetAuth(options.Credential{
		AuthSource: authDB,
		Username:   user,
		Password:   pass,
	})
	return newMongo(ctx, db, opts, maxPoolSize...)
}

func applyPoolLimit(opts *options.ClientOptions, maxPoolSize ...uint64) error {
	poolSize := config.DefaultMongoMaxPoolSize
	if len(maxPoolSize) > 1 {
		return fmt.Errorf("supply mongo pool size only once")
	}
	if len(maxPoolSize) == 1 {
		poolSize = maxPoolSize[0]
	}
	if poolSize == 0 {
		return fmt.Errorf("mongo.maxPoolSize must be a positive integer")
	}
	// Apply after the URI so it cannot bypass the application's finite cap.
	opts.SetMaxPoolSize(poolSize)
	if err := opts.Validate(); err != nil {
		return fmt.Errorf("validate mongo options: %w", err)
	}
	return nil
}

func newMongo(ctx context.Context, db string, opts *options.ClientOptions,
	maxPoolSize ...uint64,
) (*Collections, error) {
	if err := applyPoolLimit(opts, maxPoolSize...); err != nil {
		return nil, err
	}
	cl, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("connect to the db: %w", err)
	}

	if err := cl.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	appDB := cl.Database(db)

	return &Collections{
		client:                cl,
		Users:                 appDB.Collection("users"),
		PendingFriendRequests: appDB.Collection("pending_friend_requests"),
	}, nil
}
