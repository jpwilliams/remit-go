package remit

import (
	"encoding/json"
	"fmt"

	// "github.com/google/uuid"
	"github.com/chuckpreslar/emission"
	"github.com/streadway/amqp"
)

type Endpoint struct {
	RoutingKey string
	Queue      string
	session    *Session
	emitter    *emission.Emitter
	Data       chan Event
	Ready      chan bool
}

type EndpointOptions struct {
	RoutingKey string
	Queue      string
}

func createEndpoint(session *Session, options EndpointOptions) Endpoint {
	endpoint := Endpoint{
		RoutingKey: options.RoutingKey,
		Queue:      options.Queue,
		session:    session,
		emitter:    emission.NewEmitter(),
		Data:       make(chan Event),
		Ready:      make(chan bool),
	}

	go endpoint.setup()

	return endpoint
}

func (endpoint Endpoint) setup() {
	queue, err := endpoint.session.workChannel.QueueDeclare(
		endpoint.Queue, // name of the queue
		true,           // durable
		false,          // autoDelete
		false,          // exclusive
		false,          // noWait
		nil,            // arguments
	)

	failOnError(err, "Could not create endpoint queue")
	endpoint.Queue = queue.Name
	fmt.Println("Declared queue", endpoint.Queue)

	err = endpoint.session.workChannel.QueueBind(
		endpoint.Queue,      // name of the queue
		endpoint.RoutingKey, // routing key to use
		"remit",             // exchange
		false,               // noWait
		nil,                 // arguments
	)

	failOnError(err, "Could not bind queue to routing key")
	fmt.Println("Bound", endpoint.Queue, "to routing key", endpoint.RoutingKey)
	fmt.Println("Starting consumption")

	deliveries, err := endpoint.session.consumeChannel.Consume(
		endpoint.Queue, // name of the queue
		"",             // consumer tag
		false,          // noAck
		false,          // exclusive
		false,          // noLocal
		false,          // noWait
		nil,            // arguments
	)

	failOnError(err, "Failed trying to consume")
	fmt.Println("Consuming messages")

	go messageHandler(endpoint, deliveries)

	// Have made this non-blocking (so will ignore if
	// no ready listener is set up).
	// Do we want this? Or should we just return ready
	// whenever the listener is set up?
	select {
	case endpoint.Ready <- true:
	default:
		fmt.Println("No ready listener to hear")
	}
}

func (endpoint Endpoint) OnData(handler func(Event)) Endpoint {
	fmt.Println("Adding Data listener")

	go func() {
		for data := range endpoint.Data {
			handler(data)
		}
	}()

	return endpoint
}

func (endpoint Endpoint) OnReady(handler func()) Endpoint {
	fmt.Println("Adding Ready listener")

	go func() {
		for _ = range endpoint.Ready {
			handler()
		}
	}()

	return endpoint
}

func messageHandler(endpoint Endpoint, deliveries <-chan amqp.Delivery) {
	for d := range deliveries {
		parsedData := EventData{}
		err := json.Unmarshal(d.Body, &parsedData)
		failOnError(err, "Failed to parse JSON")

		event := Event{
			EventId:   d.MessageId,
			EventType: d.RoutingKey,
			Resource:  d.AppId,
			Data:      parsedData,
		}

		endpoint.Data <- event

		d.Ack(false)
	}
}