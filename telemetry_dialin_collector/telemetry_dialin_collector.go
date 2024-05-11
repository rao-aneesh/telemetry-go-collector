package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	MdtDialin "github.com/ios-xr/telemetry-go-collector/mdt_grpc_dialin"
	"github.com/ios-xr/telemetry-go-collector/telemetry_decode"
)

const tmpFileName = "telemetry-msg-*.dat"
const NotConfigured = 0xffff

var telemetryEncoding = map[string]int64{
	"gpb":                 2,
	"self-describing-gpb": 3,
	"json":                4,
}

var usage = func() {
	fmt.Fprintf(os.Stderr, "Usage: %s [options]\n", os.Args[0])

	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "Examples:\n")
	fmt.Fprintf(os.Stderr, "Subscribe                       : %s -server <ip:port> -subscription <> -encoding self-describing-gpb -username <> -password <>\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "Get proto for yang path         : %s -server <ip:port> -oper get-proto -yang <yang model or xpath> -out <filename> -username <> -password <>\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "Subscribe, using TLS            : %s -server <ip:port> -subscription <> -encoding self-describing-gpb -username <> -password <> -cert <>\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "Subscribe, use protoc to decode : %s -server <ip:port> -subscription <> -encoding gpb -username <> -password <> -proto cdp_neighbor.proto\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "Subscribe, use protoc to decode without proto: %s %s -server <ip:port> -subscription <> -encoding gpb -decode_raw\n", os.Args[0])
}

var (
	serverAddr = flag.String("server", "", "The server address, host:port")
	operation  = flag.String("oper", "subscribe", "Operation: subscribe, get-proto")
	subIds     = flag.String("subscription", "", "Subscription name to subscribe to")
	encoding   = flag.String("encoding", "json",
		"encoding to use, Options: json,self-describing-gpb,gpb")
	qos      = flag.Uint("qos", NotConfigured, "Qos to use for the session")
	yangPath = flag.String("yang_path", "", "Yang path for get-proto")
	outFile  = flag.String("out", "", "output file to write to")
	username = flag.String("username", "",
		"Username for the client connection")
	password = flag.String("password", "",
		"Password for the client connection")
	decode_raw         = flag.Bool("decode_raw", false, "Use protoc --decode_raw")
	protoFile          = flag.String("proto", "", "proto file to use for decode")
	pluginDir          = flag.String("plugin_dir", "", "absolute path to directory for proto plugins")
	pluginFile         = flag.String("plugin", "", "plugin file, used to lookup gpb symbol for decode")
	dontClean          = flag.Bool("dont_clean", false, "Don't remove tmp files on exit")
	certFile           = flag.String("cert", "", "TLS cert file")
	serverHostOverride = flag.String("server_host_override", "ems.cisco.com",
		"The server name to verify the hostname returned during TLS handshake")
	sleepPort    = flag.Uint("sleep_port", 0, "Port to listen for sleep commands")
	outputPort   = flag.Uint("output_port", 0, "Port to send the output to")
	initialSleep = flag.Uint64("initial_sleep", 0, "Initial sleep time in milliseconds")
	outputIP     = flag.String("output_ip", "", "IP to send the output to")
)

func main() {
	flag.Usage = usage
	flag.Parse()
	var opts []grpc.DialOption
	var cred passCredential

	if !*dontClean {
		// install signal handler for cleaning up tmp files
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt)
		go func() {
			<-sigs
			//cleanup()
			files, _ := filepath.Glob("/tmp/" + tmpFileName)
			for _, f := range files {
				if err := os.Remove(f); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to remove tmp file %s\n", f)
				}
			}
			os.Exit(0)
		}()
	}

	if *certFile != "" {
		var tc credentials.TransportCredentials
		tc, _ = credentials.NewClientTLSFromFile(*certFile, *serverHostOverride)
		opts = append(opts, grpc.WithTransportCredentials(tc))
	} else {
		opts = append(opts, grpc.WithInsecure())
	}
	opts = append(opts, grpc.WithPerRPCCredentials(cred))

	conn, err := grpc.Dial(*serverAddr, opts...)
	if err != nil {
		log.Fatalf("fail to dial: %v", err)
	}
	defer conn.Close()

	configOperClient := MdtDialin.NewGRPCConfigOperClient(conn)

	reqId := int64(os.Getpid())
	telemetryEncode, ok := telemetryEncoding[*encoding]
	if !ok {
		log.Fatalf("Not supported encoding: %s", *encoding)
	}
	telemetrySubIdstr := *subIds
	telemetryQos := (uint32)(*qos)

	if strings.EqualFold(*operation, "subscribe") {
		if *sleepPort != 0 {
			go sleepHandler()
		}
		var output_conn net.Conn = nil
		var err error = nil
		if *outputPort != 0 {
			output_conn, err = net.Dial("tcp", *outputIP+":"+strconv.FormatUint(uint64(*outputPort), 10))
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error opening socket to output port:", err)
				return
			}
		}
		subidstrings := strings.Split(telemetrySubIdstr, "#")

		var marking *MdtDialin.QOSMarking
		if telemetryQos != NotConfigured {
			marking = &MdtDialin.QOSMarking{Marking: telemetryQos}
		}

		//createSubsArgs := MdtDialin.CreateSubsArgs{
		//                  ReqId:         reqId,
		//                  Encode:        telemetryEncode,
		//                  Subscriptions: subidstrings,
		//                  Qos:           marking}
		//mdtSubscribe(configOperClient, &createSubsArgs)

		// let's do a session per subscription instead of
		// 1 session for all subscriptions, which above code does
		for _, subid := range subidstrings {
			createSubsArgs := MdtDialin.CreateSubsArgs{
				ReqId:    reqId,
				Encode:   telemetryEncode,
				Subidstr: subid,
				Qos:      marking}

			go mdtSubscribe(configOperClient, output_conn, &createSubsArgs)
		}
		if output_conn != nil {
			defer output_conn.Close()
		}
		select {}
	} else if strings.EqualFold(*operation, "get-proto") {
		if len(*yangPath) > 0 {
			getProtoArgs := MdtDialin.GetProtoFileArgs{ReqId: reqId, YangPath: *yangPath}
			mdtGetProto(configOperClient, &getProtoArgs)
		} else {
			fmt.Fprintln(os.Stderr, "No yang path specified!")
		}
	} else {
		fmt.Fprintln(os.Stderr, "Unsupported operation!")
	}
}

// createSubs rpc to subscribe
func mdtSubscribe(client MdtDialin.GRPCConfigOperClient, output_conn net.Conn, args *MdtDialin.CreateSubsArgs) {
	fmt.Printf("mdtSubscribe: Dialin Reqid %d subscription %s\n", args.ReqId, args.Subidstr)

	dataChan := make(chan []byte, 10000)
	//dataChan := make(chan *MdtDialin.CreateSubsReply, 10000)
	defer close(dataChan)
	//go mdtOutLoop(dataChan, args.Encode)

	o := &telemetry_decode.MdtOut{
		OutFile:    *outFile,
		Encoding:   *encoding,
		Decode_raw: *decode_raw,
		DontClean:  *dontClean,
		ProtoFile:  *protoFile,
		PluginDir:  *pluginDir,
		PluginFile: *pluginFile,
		DataChan:   dataChan,
	}
	// handler for decoding the data, reads data from dataChan
	go o.MdtOutLoop(output_conn)

	stream, err := client.CreateSubs(context.Background(), args)
	if err != nil {
		log.Fatalf("mdtSubscribe: ReqId %d, %v", args.ReqId, err)
	}

	for {
		reply, err := stream.Recv()
		time.Sleep(time.Duration(*initialSleep) * time.Millisecond) // Add a sleep to slow down processing

		if err == io.EOF {
			fmt.Printf("Subscribe: Got EOF\n\n")
			break
		}
		if err != nil {
			log.Fatalf("Subscribe: ReqId %d, %v", args.ReqId, err)
		}

		if len(reply.Data) == 0 {
			if len(reply.Errors) != 0 {
				fmt.Fprintf(os.Stderr, "Subscribe: Received ReqId %d, error:\n%s\n", args.ReqId, reply.Errors)
				os.Exit(1)
			}
		} else {
			dataChan <- reply.Data
		}
	}

}

// Get Proto request
func mdtGetProto(client MdtDialin.GRPCConfigOperClient, args *MdtDialin.GetProtoFileArgs) int64 {
	var oFile *os.File

	stream, err := client.GetProtoFile(context.Background(), args)
	if err != nil {
		log.Fatalf("GetProto: ReqId %d, %v", args.ReqId, err)
		return 0
	}

	oFile = os.Stdout
	if len(*outFile) != 0 {
		oFile, err = os.Create(*outFile)
		defer oFile.Close()
	}

	for {
		reply, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Fatalf("GetProto: ReqId %d, %v", args.ReqId, err)
			return 0
		}

		if len(reply.Errors) != 0 {
			fmt.Fprintf(os.Stderr, "GetProto: ReqId %d, received error: %s\n", args.ReqId, reply.Errors)
			return 0
		} else if reply.ReqId != args.ReqId {
			fmt.Fprintf(os.Stderr, "GetProto: mismatch sent ReqID %d, Received ReqId %d\n", args.ReqId, reply.ReqId)
			return 0
		} else {
			if len(reply.ProtoContent) == 0 {
				fmt.Printf("GetProto: Received ReqId %d \n", reply.ReqId)
			} else {
				_, err := oFile.WriteString(reply.ProtoContent)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			}
		}
	}

	return 0
}

// Server for handling commands
func sleepHandler() {
	// Listen on TCP port
	ip_address := "0.0.0.0"
	if *outputIP == "" {
		ip_address = "localhost"
	}
	ln, err := net.Listen("tcp", ip_address+":"+strconv.FormatUint(uint64(*sleepPort), 10))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error listening:", err.Error())
		os.Exit(1)
	}
	defer ln.Close()

	fmt.Printf("sleepHandler is listening on %s...\n", ln.Addr().String())

	for {
		// Accept connection on port
		conn, err := ln.Accept()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error accepting:", err.Error())
			continue
		}

		// Create a buffer reader for the connection
		reader := bufio.NewReader(conn)

		for {
			// Read message from the connection
			message, err := reader.ReadString('\n')
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error reading:", err.Error())
				break
			}

			trimmedMessage := strings.TrimSpace(message) // Remove newline and whitespace

			// Check for CLOSE message
			if trimmedMessage == "CLOSE" {
				fmt.Println("Received CLOSE message, closing connection.")
				conn.Write([]byte("Closing connection.\n"))
				os.Exit(0)
			}

			// Convert message to uint64
			number, err := strconv.ParseUint(trimmedMessage, 10, 64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error converting message to uint64: %s\n", err.Error())
				continue
			}

			*initialSleep = number

			// Send a response
			response := fmt.Sprintf("SLEEP_TIMER set to: %d\n", *initialSleep)
			conn.Write([]byte(response))
		}

		// Close the current connection before accepting a new one
		conn.Close()
		fmt.Println("Connection closed. Ready to accept a new connection.")
	}
}

type passCredential int

func (passCredential) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{
		"username": *username,
		"password": *password,
	}, nil
}

func (passCredential) RequireTransportSecurity() bool {
	return false
}
