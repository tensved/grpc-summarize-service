package main

import (
	"fmt"
	"log"
	"net"
	"net/http"

	"grpc-summarize-service/config"
	"grpc-summarize-service/server"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

func main() {
	config.Init()

	lis, err := net.Listen("tcp", ":"+config.Global.GRPCPort)
	if err != nil {
		log.Fatalf("Failed to listen on port %s: %v", config.Global.GRPCPort, err)
	}

	grpcServer := grpc.NewServer()

	// Register reflection API for grpcurl
	reflection.Register(grpcServer)

	// Register gRPC health check service
	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)

	summarizerServer := server.NewSummarizerServer()
	summarizerServer.RegisterServer(grpcServer)

	// Start HTTP server for health check
	go func() {
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		})

		httpServer := &http.Server{
			Addr:    ":" + config.Global.HealthPort,
			Handler: http.DefaultServeMux,
		}

		fmt.Printf("Health check server is running on port %s\n", config.Global.HealthPort)

		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	fmt.Printf("Listening on %s\n", lis.Addr())
	fmt.Printf("AI Service URL: %s\n", config.Global.AIServiceURL)
	fmt.Printf("AI Model: %s\n", config.Global.AIModel)
	fmt.Printf("Max Messages: %d\n", config.Global.MaxMessages)
	fmt.Printf("Log Level: %s\n", config.Global.LogLevel)

	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve gRPC server: %v", err)
	}
}
