package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"

	"github.com/BananaLabs-OSS/Fiber/pulp"
	"github.com/BananaLabs-OSS/Fiber/pulp/cellconfig"
	pulpgin "github.com/BananaLabs-OSS/Fiber/pulp/gin"
	"github.com/BananaLabs-OSS/Fiber/pulp/workflow"
)

const orchestratorCell = "lua-orchestrator"

func main() {}

func init() { pulp.OnInit(bootstrap) }

type config struct {
	ServiceToken string `json:"service_token"`
}

type response struct {
	Status int               `msgpack:"status"`
	Body   string            `msgpack:"body"`
	Error  string            `msgpack:"error"`
	Env    map[string]string `msgpack:"env"`
}

func bootstrap(configBytes []byte) error {
	var cfg config
	if len(configBytes) > 0 {
		if err := cellconfig.Decode(configBytes, &cfg); err != nil {
			return fmt.Errorf("decode config: %w", err)
		}
	}
	client := workflow.NewClient(orchestratorCell)
	if _, err := client.Dispatch(workflow.DispatchRequest{Event: "hytale-auth.init.v1"}); err != nil {
		return fmt.Errorf("initialize application: %w", err)
	}
	engine := pulpgin.New()
	engine.GET("/health", func(c *pulpgin.Context) { textEvent(c, client, "hytale-auth.http.health.v1") })
	engine.GET("/", func(c *pulpgin.Context) { textEvent(c, client, "hytale-auth.http.status.v1") })
	engine.GET("/tokens", func(c *pulpgin.Context) {
		if cfg.ServiceToken != "" && !sameToken(c.GetHeader("X-Service-Token"), cfg.ServiceToken) {
			httpError(c, 401, "unauthorized")
			return
		}
		result, err := dispatch(client, "hytale-auth.http.tokens.v1", nil)
		if err != nil {
			httpError(c, 500, err.Error())
			return
		}
		if result.Status >= 400 {
			httpError(c, result.Status, result.Error)
			return
		}
		body, err := json.Marshal(map[string]any{"env": result.Env})
		if err != nil {
			httpError(c, 500, err.Error())
			return
		}
		c.Data(200, "text/plain; charset=utf-8", append(body, '\n'))
	})
	if err := engine.RegisterRoutes(); err != nil {
		return err
	}
	pulp.OnStep(func(event pulp.StepEvent) error {
		if _, err := client.Dispatch(workflow.DispatchRequest{Event: "hytale-auth.tick.v1", Payload: map[string]any{
			"wall_nanos": fmt.Sprint(event.WallTime),
		}}); err != nil {
			log.Printf("device authorization tick failed: %v", err)
		}
		return engine.Dispatch(event)
	})
	return nil
}

func dispatch(client *workflow.Client, event string, payload any) (response, error) {
	result, err := client.Dispatch(workflow.DispatchRequest{Event: event, Payload: payload})
	if err != nil {
		return response{}, err
	}
	return workflow.DecodeValue[response](result)
}

func textEvent(c *pulpgin.Context, client *workflow.Client, event string) {
	result, err := dispatch(client, event, nil)
	if err != nil {
		httpError(c, 500, err.Error())
		return
	}
	c.Data(result.Status, "text/plain; charset=utf-8", []byte(result.Body))
}

func httpError(c *pulpgin.Context, status int, message string) {
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(status, "text/plain; charset=utf-8", []byte(message+"\n"))
}

func sameToken(got, want string) bool {
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
