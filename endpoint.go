package remit

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/oklog/ulid"
	"github.com/streadway/amqp"
)

// Endpoint manages the RPC-style consumption and
// publishing of messages.
type Endpoint struct {
	session       *Session
	channel       *amqp.Channel
	workChannel   *amqp.Channel
	waitGroup     *sync.WaitGroup
	mu            *sync.Mutex
	consumerTag   string
	dataListeners []chan Event

	RoutingKey string
	Queue      string

	Data  chan Event
	Ready chan bool

	DataHandler EndpointDataHandler
}

type EndpointOptions struct {
	RoutingKey string
	Queue      string

	DataHandler EndpointDataHandler
}

type EndpointDataHandler func(Event)

func createEndpoint(session *Session, options EndpointOptions) Endpoint {
	debug("creating endpoint")

	endpoint := Endpoint{
		RoutingKey: options.RoutingKey,
		Queue:      options.Queue,
		session:    session,
		Data:       make(chan Event),
		waitGroup:  &sync.WaitGroup{},
		mu:         &sync.Mutex{},
	}

	return endpoint
}

func (endpoint *Endpoint) getWorkChannel() *amqp.Channel {
	endpoint.mu.Lock()
	defer endpoint.mu.Unlock()

	if endpoint.workChannel != nil {
		return endpoint.workChannel
	}

	var err error
	endpoint.workChannel, err = endpoint.session.connection.Channel()
	failOnError(err, "Failed to create work channel for endpoint")

	go func() {
		waitForClose := make(chan *amqp.Error, 0)
		endpoint.workChannel.NotifyClose(waitForClose)
		<-waitForClose
		endpoint.workChannel = nil
		endpoint.getWorkChannel()
	}()

	return endpoint.workChannel
}

func (endpoint *Endpoint) Open() Endpoint {
	debug("opening endpoint; declaring queue")

	queue, err := endpoint.getWorkChannel().QueueDeclare(
		endpoint.Queue, // name of the queue
		true,           // durable
		false,          // autoDelete
		false,          // exclusive
		false,          // noWait
		nil,            // arguments
	)

	failOnError(err, "Could not create endpoint queue")
	endpoint.Queue = queue.Name

	debug("opening endpoint; binding queue")
	err = endpoint.getWorkChannel().QueueBind(
		endpoint.Queue,      // name of the queue
		endpoint.RoutingKey, // routing key to use
		"remit",             // exchange
		false,               // noWait
		nil,                 // arguments
	)

	failOnError(err, "Could not bind queue to routing key")

	debug("opening endpoint; setting endpoint channel")
	endpoint.channel, err = endpoint.session.connection.Channel()
	failOnError(err, "Failed to create channel for consumption")

	debug("opening endpoint; consuming")
	endpoint.consumerTag = ulid.MustNew(ulid.Now(), nil).String()
	deliveries, err := endpoint.channel.Consume(
		endpoint.Queue,       // name of the queue
		endpoint.consumerTag, // consumer tag
		false,                // noAck
		false,                // exclusive
		false,                // noLocal
		false,                // noWait
		nil,                  // arguments
	)

	failOnError(err, "Failed trying to consume")

	go messageHandler(*endpoint, deliveries)

	debug("endpoint opened")

	// Have made this non-blocking (so will ignore if
	// no ready listener is set up).
	// Do we want this? Or should we just return ready
	// whenever the listener is set up?
	select {
	case endpoint.Ready <- true:
	default:
	}

	return *endpoint
}

func (endpoint *Endpoint) OnData(handlers ...EndpointDataHandler) Endpoint {
	if len(handlers) == 0 {
		panic("Failed to create endpoint data handler with no functions")
	}

	dataChan := make(chan Event)
	endpoint.mu.Lock()
	endpoint.dataListeners = append(endpoint.dataListeners, dataChan)
	endpoint.mu.Unlock()

	go func() {
		for event := range dataChan {
			go handleData(*endpoint, handlers, &event)
		}
	}()

	return *endpoint
}

func (endpoint Endpoint) Close() {
	err := endpoint.channel.Cancel(endpoint.consumerTag, false)
	failOnError(err, "Failed to clean up consume channel handler")
	endpoint.waitGroup.Wait()
	err = endpoint.channel.Close()
	failOnError(err, "Failed to close consume channel for endpoint")
	close(endpoint.Data)
	close(endpoint.Ready)
}

func handleData(endpoint Endpoint, handlers []EndpointDataHandler, event *Event) {
	endpoint.session.waitGroup.Add(1)
	defer endpoint.session.waitGroup.Done()
	endpoint.waitGroup.Add(1)
	defer endpoint.waitGroup.Done()
	event.waitGroup.Add(1)
	defer event.waitGroup.Done()

	var retResult interface{}
	var retErr interface{}

runner:
	for _, handler := range handlers {
		go handler(*event)

		select {
		case retResult = <-event.Success:
			break runner
		case retErr = <-event.Failure:
			break runner
		case <-event.Next:
		}
	}

	if retErr != nil {
		debug("failure" + event.message.MessageId)
	} else {
		debug("success " + event.message.MessageId)
	}

	var accumulatedResults [2]interface{}
	accumulatedResults[0] = retErr
	accumulatedResults[1] = retResult

	j, err := json.Marshal(accumulatedResults)
	failOnError(err, "Failed making JSON from result")

	if event.message.ReplyTo == "" || event.message.CorrelationId == "" {
		event.message.Ack(false)
		return
	}

	queue, err := endpoint.getWorkChannel().QueueDeclarePassive(
		event.message.ReplyTo, // the queue to assert
		false, // durable
		true,  // autoDelete
		true,  // exclusive
		false, // noWait
		nil,   // arguments
	)

	if err != nil {
		fmt.Println("Reply consumer no longer present; skipping")
		event.message.Ack(false)
		return
	}

	err = endpoint.session.publishChannel.Publish(
		"",         // exchange - use default here to publish directly to queue
		queue.Name, // routing key / queue
		false,      // mandatory
		false,      // immediate
		amqp.Publishing{
			Headers:       amqp.Table{},
			ContentType:   "application/json",
			Body:          j,
			Timestamp:     time.Now(),
			MessageId:     ulid.MustNew(ulid.Now(), nil).String(),
			AppId:         endpoint.session.Config.Name,
			CorrelationId: event.message.CorrelationId,
		},
	)

	failOnError(err, "Couldn't send that message")

	event.message.Ack(false)
}

func messageHandler(endpoint Endpoint, deliveries <-chan amqp.Delivery) {
	for d := range deliveries {
		parsedData := EventData{}
		err := json.Unmarshal(d.Body, &parsedData)
		if err != nil {
			fmt.Println("Failed to parse JSON " + d.MessageId)
			fmt.Println(err)
			d.Nack(false, false)
			continue
		}

		event := Event{
			EventId:   d.MessageId,
			EventType: d.RoutingKey,
			Resource:  d.AppId,
			Data:      parsedData,
			Success:   make(chan interface{}, 1),
			Failure:   make(chan interface{}, 1),
			Next:      make(chan bool, 1),

			message:   d,
			waitGroup: &sync.WaitGroup{},
		}

		event.waitGroup.Add(len(endpoint.dataListeners))

		go func() {
			event.waitGroup.Wait()
			close(event.Success)
			close(event.Failure)
			close(event.Next)
		}()

		for _, listener := range endpoint.dataListeners {
			select {
			case listener <- event:
			default:
			}
		}
	}
}
