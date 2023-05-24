package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	radix "github.com/mediocregopher/radix/v4"
	"github.com/redis/go-redis/v9"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"
)

var totalMessages uint64

type testResult struct {
	StartTime             int64     `json:"StartTime"`
	Duration              float64   `json:"Duration"`
	Mode                  string    `json:"Mode"`
	MessageRate           float64   `json:"MessageRate"`
	TotalMessages         uint64    `json:"TotalMessages"`
	TotalSubscriptions    int       `json:"TotalSubscriptions"`
	ChannelMin            int       `json:"ChannelMin"`
	ChannelMax            int       `json:"ChannelMax"`
	SubscribersPerChannel int       `json:"SubscribersPerChannel"`
	MessagesPerChannel    int64     `json:"MessagesPerChannel"`
	MessageRateTs         []float64 `json:"MessageRateTs"`
	OSSDistributedSlots   bool      `json:"OSSDistributedSlots"`
	Addresses             []string  `json:"Addresses"`
}

func subscriberRoutine(addr string, mode, subscriberName string, channel string, printMessages bool, ctx context.Context, wg *sync.WaitGroup, opts radix.Dialer, protocolVersion int) {
	// tell the caller we've stopped
	defer wg.Done()
	client := redis.NewClient(&redis.Options{
		Addr:            addr,
		Password:        opts.AuthPass,
		ClientName:      subscriberName,
		ProtocolVersion: protocolVersion,
	})
	switch mode {
	case "ssubscribe":
		spubsub := client.SSubscribe(ctx, channel)
		defer spubsub.Close()
		for {
			msg, err := spubsub.ReceiveMessage(ctx)
			if err != nil {
				panic(err)
			}
			if printMessages {
				fmt.Println(fmt.Sprintf("received message in channel %s. Message: %s", msg.Channel, msg.Payload))
			}
			atomic.AddUint64(&totalMessages, 1)
		}
		break
	case "subscribe":
		fallthrough
	default:
		pubsub := client.Subscribe(ctx, channel)
		defer pubsub.Close()
		for {
			msg, err := pubsub.ReceiveMessage(ctx)
			if err != nil {
				panic(err)
			}
			if printMessages {
				fmt.Println(fmt.Sprintf("received message in channel %s. Message: %s", msg.Channel, msg.Payload))
			}
			atomic.AddUint64(&totalMessages, 1)
		}
	}

}

func main() {
	host := flag.String("host", "127.0.0.1", "redis host.")
	port := flag.String("port", "6379", "redis port.")
	cpuprofile := flag.String("cpuprofile", "", "write cpu profile to file")
	password := flag.String("a", "", "Password for Redis Auth.")
	mode := flag.String("mode", "subscribe", "Subscribe mode. Either 'subscribe' or 'ssubscribe'.")
	username := flag.String("user", "", "Used to send ACL style 'AUTH username pass'. Needs -a.")
	subscribers_placement := flag.String("subscribers-placement-per-channel", "dense", "(dense,sparse) dense - Place all subscribers to channel in a specific shard. sparse- spread the subscribers across as many shards possible, in a round-robin manner.")
	channel_minimum := flag.Int("channel-minimum", 1, "channel ID minimum value ( each channel has a dedicated thread ).")
	channel_maximum := flag.Int("channel-maximum", 100, "channel ID maximum value ( each channel has a dedicated thread ).")
	subscribers_per_channel := flag.Int("subscribers-per-channel", 1, "number of subscribers per channel.")
	messages_per_channel_subscriber := flag.Int64("messages", 0, "Number of total messages per subscriber per channel.")
	json_out_file := flag.String("json-out-file", "", "Name of json output file, if not set, will not print to json.")
	client_update_tick := flag.Int("client-update-tick", 1, "client update tick.")
	test_time := flag.Int("test-time", 0, "Number of seconds to run the test, after receiving the first message.")
	subscribe_prefix := flag.String("subscriber-prefix", "channel-", "prefix for subscribing to channel, used in conjunction with key-minimum and key-maximum.")
	client_output_buffer_limit_pubsub := flag.String("client-output-buffer-limit-pubsub", "", "Specify client output buffer limits for clients subscribed to at least one pubsub channel or pattern. If the value specified is different that the one present on the DB, this setting will apply.")
	distributeSubscribers := flag.Bool("oss-cluster-api-distribute-subscribers", false, "read cluster slots and distribute subscribers among them.")
	printMessages := flag.Bool("print-messages", false, "print messages.")
	//TODO FIX ME
	//dialTimeout := flag.Duration("redis-timeout", time.Second*300, "determines the timeout to pass to redis connection setup. It adjust the connection, read, and write timeouts.")
	resp := flag.Int("resp", 2, "redis command response protocol (2 - RESP 2, 3 - RESP 3)")
	flag.Parse()
	totalMessages = 0
	var nodes []radix.ClusterNode
	var nodesAddresses []string
	var node_subscriptions_count []int
	opts := radix.Dialer{}
	if *password != "" {
		opts.AuthPass = *password
		if *username != "" {
			opts.AuthUser = *username
		}
	}
	if *resp == 2 {
		opts.Protocol = "2"
	} else if *resp == 3 {
		opts.Protocol = "3"
	}

	if *test_time != 0 && *messages_per_channel_subscriber != 0 {
		log.Fatal(fmt.Errorf("--messages and --test-time are mutially exclusive ( please specify one or the other )"))
	}

	if *distributeSubscribers {
		nodes, nodesAddresses, node_subscriptions_count = getClusterNodesFromTopology(host, port, nodes, nodesAddresses, node_subscriptions_count, opts)
	} else {
		nodes, nodesAddresses, node_subscriptions_count = getClusterNodesFromArgs(nodes, port, host, nodesAddresses, node_subscriptions_count)
	}

	if strings.Compare(*client_output_buffer_limit_pubsub, "") != 0 {
		checkClientOutputBufferLimitPubSub(nodes, client_output_buffer_limit_pubsub, opts)
	}

	ctx := context.Background()
	// trap Ctrl+C and call cancel on the context
	// We Use this instead of the previous stopChannel + chan radix.PubSubMessage
	ctx, cancel := context.WithCancel(ctx)
	cS := make(chan os.Signal, 1)
	signal.Notify(cS, os.Interrupt)
	defer func() {
		signal.Stop(cS)
		cancel()
	}()
	go func() {
		select {
		case <-cS:
			cancel()
		case <-ctx.Done():
		}
	}()

	// a WaitGroup for the goroutines to tell us they've stopped
	wg := sync.WaitGroup{}
	total_channels := *channel_maximum - *channel_minimum + 1
	subscriptions_per_node := total_channels / len(nodes)
	total_subscriptions := total_channels * *subscribers_per_channel
	total_messages := int64(total_subscriptions) * *messages_per_channel_subscriber
	fmt.Println(fmt.Sprintf("Total subcriptions: %d. Subscriptions per node %d. Total messages: %d", total_subscriptions, subscriptions_per_node, total_messages))
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
	}

	if strings.Compare(*subscribers_placement, "dense") == 0 {
		for channel_id := *channel_minimum; channel_id <= *channel_maximum; channel_id++ {
			for channel_subscriber_number := 1; channel_subscriber_number <= *subscribers_per_channel; channel_subscriber_number++ {
				nodes_pos := channel_id % len(nodes)
				node_subscriptions_count[nodes_pos]++
				addr := nodes[nodes_pos]
				channel := fmt.Sprintf("%s%d", *subscribe_prefix, channel_id)
				subscriberName := fmt.Sprintf("subscriber#%d-%s%d", channel_subscriber_number, *subscribe_prefix, channel_id)
				wg.Add(1)
				go subscriberRoutine(addr.Addr, *mode, subscriberName, channel, *printMessages, ctx, &wg, opts, *resp)
			}
		}
	}

	// listen for C-c
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	w := new(tabwriter.Writer)

	tick := time.NewTicker(time.Duration(*client_update_tick) * time.Second)
	closed, start_time, duration, totalMessages, messageRateTs := updateCLI(tick, c, total_messages, w, *test_time)
	messageRate := float64(totalMessages) / float64(duration.Seconds())

	if *cpuprofile != "" {
		pprof.StopCPUProfile()
	}

	fmt.Fprint(w, fmt.Sprintf("#################################################\nTotal Duration %f Seconds\nMessage Rate %f\n#################################################\n", duration.Seconds(), messageRate))
	fmt.Fprint(w, "\r\n")
	w.Flush()

	if strings.Compare(*json_out_file, "") != 0 {

		res := testResult{
			StartTime:             start_time.Unix(),
			Duration:              duration.Seconds(),
			Mode:                  *mode,
			MessageRate:           messageRate,
			TotalMessages:         totalMessages,
			TotalSubscriptions:    total_subscriptions,
			ChannelMin:            *channel_minimum,
			ChannelMax:            *channel_maximum,
			SubscribersPerChannel: *subscribers_per_channel,
			MessagesPerChannel:    *messages_per_channel_subscriber,
			MessageRateTs:         messageRateTs,
			OSSDistributedSlots:   *distributeSubscribers,
			Addresses:             nodesAddresses,
		}
		file, err := json.MarshalIndent(res, "", " ")
		if err != nil {
			log.Fatal(err)
		}

		err = ioutil.WriteFile(*json_out_file, file, 0644)
		if err != nil {
			log.Fatal(err)
		}
	}

	if closed {
		return
	}

	// tell the goroutine to stop
	close(c)
	// and wait for them both to reply back
	wg.Wait()
}

func getClusterNodesFromArgs(nodes []radix.ClusterNode, port *string, host *string, nodesAddresses []string, node_subscriptions_count []int) ([]radix.ClusterNode, []string, []int) {
	nodes = []radix.ClusterNode{}
	ports := strings.Split(*port, ",")
	for idx, nhost := range strings.Split(*host, ",") {
		node := radix.ClusterNode{
			Addr:            fmt.Sprintf("%s:%s", nhost, ports[idx]),
			ID:              "",
			Slots:           nil,
			SecondaryOfAddr: "",
			SecondaryOfID:   "",
		}
		nodes = append(nodes, node)
		nodesAddresses = append(nodesAddresses, node.Addr)
		node_subscriptions_count = append(node_subscriptions_count, 0)
	}
	return nodes, nodesAddresses, node_subscriptions_count
}

func getClusterNodesFromTopology(host *string, port *string, nodes []radix.ClusterNode, nodesAddresses []string, node_subscriptions_count []int, opts radix.Dialer) ([]radix.ClusterNode, []string, []int) {
	// Create a normal redis connection
	ctx := context.Background()
	conn, err := opts.Dial(ctx, "tcp", fmt.Sprintf("%s:%s", *host, *port))
	if err != nil {
		panic(err)
	}
	var topology radix.ClusterTopo
	err = conn.Do(ctx, radix.FlatCmd(&topology, "CLUSTER", "SLOTS"))
	if err != nil {
		log.Fatal(err)
	}

	for _, slot := range topology.Map() {
		slot_host := strings.Split(slot.Addr, ":")[0]
		slot_port := strings.Split(slot.Addr, ":")[1]
		if strings.Compare(slot_host, "127.0.0.1") == 0 {
			slot.Addr = fmt.Sprintf("%s:%s", *host, slot_port)
		}
		nodes = append(nodes, slot)
		nodesAddresses = append(nodesAddresses, slot.Addr)
		node_subscriptions_count = append(node_subscriptions_count, 0)
	}
	conn.Close()
	return nodes, nodesAddresses, node_subscriptions_count
}

func updateCLI(tick *time.Ticker, c chan os.Signal, message_limit int64, w *tabwriter.Writer, test_time int) (bool, time.Time, time.Duration, uint64, []float64) {

	start := time.Now()
	prevTime := time.Now()
	prevMessageCount := uint64(0)
	messageRateTs := []float64{}

	w.Init(os.Stdout, 25, 0, 1, ' ', tabwriter.AlignRight)
	fmt.Fprint(w, fmt.Sprintf("Test Time\tTotal Messages\t Message Rate \t"))
	fmt.Fprint(w, "\n")
	w.Flush()
	for {
		select {
		case <-tick.C:
			{
				now := time.Now()
				took := now.Sub(prevTime)
				messageRate := float64(totalMessages-prevMessageCount) / float64(took.Seconds())
				if prevMessageCount == 0 && totalMessages != 0 {
					start = time.Now()
				}
				if totalMessages != 0 {
					messageRateTs = append(messageRateTs, messageRate)
				}
				prevMessageCount = totalMessages
				prevTime = now

				fmt.Fprint(w, fmt.Sprintf("%.0f\t%d\t%.2f\t", time.Since(start).Seconds(), totalMessages, messageRate))
				fmt.Fprint(w, "\r\n")
				w.Flush()
				if message_limit > 0 && totalMessages >= uint64(message_limit) {
					return true, start, time.Since(start), totalMessages, messageRateTs
				}
				if test_time > 0 && time.Since(start) >= time.Duration(test_time*1000*1000*1000) && totalMessages != 0 {
					return true, start, time.Since(start), totalMessages, messageRateTs
				}

				break
			}

		case <-c:
			fmt.Println("received Ctrl-c - shutting down")
			return true, start, time.Since(start), totalMessages, messageRateTs
		}
	}
	return false, start, time.Since(start), totalMessages, messageRateTs
}

func checkClientOutputBufferLimitPubSub(nodes []radix.ClusterNode, client_output_buffer_limit_pubsub *string, opts radix.Dialer) {
	for _, slot := range nodes {
		ctx := context.Background()
		conn, err := opts.Dial(ctx, "tcp", slot.Addr)
		if err != nil {
			panic(err)
		}
		_, err, pubsubTopology := getPubSubBufferLimit(err, conn)
		if strings.Compare(*client_output_buffer_limit_pubsub, pubsubTopology) != 0 {
			fmt.Println(fmt.Sprintf("\tCHANGING DB pubsub topology for address %s from %s to %s", slot.Addr, pubsubTopology, *client_output_buffer_limit_pubsub))

			err = conn.Do(ctx, radix.FlatCmd(nil, "CONFIG", "SET", "client-output-buffer-limit", fmt.Sprintf("pubsub %s", *client_output_buffer_limit_pubsub)))
			if err != nil {
				log.Fatal(err)
			}
			_, err, pubsubTopology = getPubSubBufferLimit(err, conn)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Println(fmt.Sprintf("\tCHANGED DB pubsub topology for address %s: %s", slot.Addr, pubsubTopology))
		} else {
			fmt.Println(fmt.Sprintf("\tNo need to change pubsub topology for address %s: %s", slot.Addr, pubsubTopology))
		}
		conn.Close()
	}
}

func getPubSubBufferLimit(err error, conn radix.Conn) ([]string, error, string) {
	var topologyResponse []string
	ctx := context.Background()
	err = conn.Do(ctx, radix.FlatCmd(&topologyResponse, "CONFIG", "GET", "client-output-buffer-limit"))
	if err != nil {
		log.Fatal(err)
	}
	i := strings.Index(topologyResponse[1], "pubsub ")
	pubsubTopology := topologyResponse[1][i+7:]
	return topologyResponse, err, pubsubTopology
}
