// TODO
// * syntax check mig.Action.Arguments before exec()
/* Mozilla InvestiGator Agent

Version: MPL 1.1/GPL 2.0/LGPL 2.1

The contents of this file are subject to the Mozilla Public License Version
1.1 (the "License"); you may not use this file except in compliance with
the License. You may obtain a copy of the License at
http://www.mozilla.org/MPL/

Software distributed under the License is distributed on an "AS IS" basis,
WITHOUT WARRANTY OF ANY KIND, either express or implied. See the License
for the specific language governing rights and limitations under the
License.

The Initial Developer of the Original Code is
Mozilla Corporation
Portions created by the Initial Developer are Copyright (C) 2013
the Initial Developer. All Rights Reserved.

Contributor(s):
Julien Vehent jvehent@mozilla.com [:ulfr]

Alternatively, the contents of this file may be used under the terms of
either the GNU General Public License Version 2 or later (the "GPL"), or
the GNU Lesser General Public License Version 2.1 or later (the "LGPL"),
in which case the provisions of the GPL or the LGPL are applicable instead
of those above. If you wish to allow use of your version of this file only
under the terms of either the GPL or the LGPL, and not to allow others to
use your version of this file under the terms of the MPL, indicate your
decision by deleting the provisions above and replace them with the notice
and other provisions required by the GPL or the LGPL. If you do not delete
the provisions above, a recipient may use your version of this file under
the terms of any one of the MPL, the GPL or the LGPL.
*/
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/streadway/amqp"
	"mig"
	"mig/modules/filechecker"
	"mig/pgp"
	"os"
	"os/exec"
	"strings"
	"time"
)

// build version
var version string

func main() {
	// parse command line argument
	// -m selects the mode {agent, filechecker, ...}
	var mode = flag.String("m", "agent", "module to run (eg. agent, filechecker)")
	flag.Parse()


	switch *mode {

	case "filechecker":
		// pass the rest of the arguments as a byte array
		// to the filechecker module
		var tmparg string
		for _, arg := range flag.Args() {
			tmparg = tmparg + arg
		}
		args := []byte(tmparg)
		fmt.Printf(filechecker.Run(args))
		os.Exit(0)

	case "agent":
		var ctx Context
		var err error

		// if init fails, sleep for one minute and try again. forever.
		for {
			ctx, err = Init()
			if err == nil {
				break
			}
			fmt.Println(err)
			fmt.Println("initialisation failed. sleep and retry.");
			time.Sleep(60 * time.Second)
		}

		// Goroutine that receives messages from AMQP
		go getCommands(ctx)

		// GoRoutine that parses and validates incoming commands
		go func(){
			for msg := range ctx.Channels.NewCommand {
				err = parseCommands(ctx, msg)
				if err != nil {
					log := mig.Log{Desc: fmt.Sprintf("%v", err)}.Err()
					ctx.Channels.Log <- log
				}
			}
		}()

		// GoRoutine that executes commands that run as agent modules
		go func(){
			for cmd := range ctx.Channels.RunAgentCommand {
				err = runAgentModule(ctx, cmd)
				if err != nil {
					log := mig.Log{CommandID: cmd.ID, ActionID: cmd.Action.ID, Desc: fmt.Sprintf("%v", err)}.Err()
					ctx.Channels.Log <- log
				}
			}
		}()

		// GoRoutine that formats results and send them to scheduler
		go func() {
			for result := range ctx.Channels.Results {
				err = sendResults(ctx, result)
				if err != nil {
					// on failure, log and attempt to report it to the scheduler
					log := mig.Log{CommandID: result.ID, ActionID: result.Action.ID, Desc: fmt.Sprintf("%v", err)}.Err()
					ctx.Channels.Log <- log
				}
			}
		}()

		// GoRoutine that sends keepAlive messages to scheduler
		go keepAliveAgent(ctx)

		ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("Mozilla InvestiGator version %s: started agent %s", version, ctx.Agent.QueueLoc)}

		// won't exit until this chan received something
		exitReason := <-ctx.Channels.Terminate
		ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("Shutting down agent: '%v'", exitReason)}.Emerg()
		Destroy(ctx)
	}
}

// getCommands receives AMQP messages, and feed them to the action chan
func getCommands(ctx Context) (err error) {
	for m := range ctx.MQ.Bind.Chan {
		ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("received message '%s'", m.Body)}.Debug()

		// Ack this message only
		err := m.Ack(true)
		if err != nil {
			desc := fmt.Sprintf("Failed to acknowledge reception. Message will be ignored. Body: '%s'", m.Body)
			ctx.Channels.Log <- mig.Log{Desc: desc}.Err()
			continue
		}

		// pass it along
		ctx.Channels.NewCommand <- m.Body
		ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("received message. queued in position %d", len(ctx.Channels.NewCommand))}
	}
	return
}

// parseCommands transforms a message into a MIG Command struct, performs validation
// and run the command
func parseCommands(ctx Context, msg []byte) (err error) {
	var cmd mig.Command
	cmd.ID = 0	// safety net
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("parseCommands() -> %v", e)

			// if we have a command to return, update status and send back
			if cmd.ID > 0 {
				cmd.Results = mig.Log{CommandID: cmd.ID, ActionID: cmd.Action.ID, Desc: fmt.Sprintf("%v", err)}.Err()
				cmd.Status = "failed"
				ctx.Channels.Results <- cmd
			}
		}
		ctx.Channels.Log <- mig.Log{CommandID: cmd.ID, ActionID: cmd.Action.ID, Desc: "leaving parseCommands()"}.Debug()
	}()

	// unmarshal the received command into a command struct
	// if this fails, inform the scheduler and skip this message
	err = json.Unmarshal(msg, &cmd)
	if err != nil {
		panic(err)
	}

	// get an io.Reader from the public pgp key
	keyring, err := pgp.TransformArmoredPubKeyToKeyring(PUBLICPGPKEY)
	if err != nil {
		panic(err)
	}

	// Check the action syntax and signature
	err = cmd.Action.Validate(keyring)
	if err != nil {
		desc := fmt.Sprintf("action validation failed: %v", err)
		ctx.Channels.Log <- mig.Log{CommandID: cmd.ID, ActionID: cmd.Action.ID, Desc: desc}.Err()
		panic(desc)
	}

	// Expiration is verified by the Validate() call above, but we need
	// to verify the ScheduledDate ourselves
	if time.Now().Before(cmd.Action.ScheduledDate) {
		ctx.Channels.Log <- mig.Log{CommandID: cmd.ID, ActionID: cmd.Action.ID, Desc: "action is scheduled for later"}.Err()
		panic("ScheduledDateInFuture")
	}

	switch cmd.Action.Order {
	case "filechecker":
		// send to the agent module execution path
		ctx.Channels.RunAgentCommand <- cmd
		ctx.Channels.Log <- mig.Log{CommandID: cmd.ID, ActionID: cmd.Action.ID, Desc: "Command queued for execution"}
	case "shell":
		// send to the external command execution path
		ctx.Channels.RunExternalCommand <- cmd
		ctx.Channels.Log <- mig.Log{CommandID: cmd.ID, ActionID: cmd.Action.ID, Desc: "Command queued for execution"}
	case "terminate":
		ctx.Channels.Terminate <- fmt.Errorf("Terminate order received from scheduler")
	default:
		ctx.Channels.Log <- mig.Log{CommandID: cmd.ID, ActionID: cmd.Action.ID, Desc: fmt.Sprintf("order '%s' is invalid", cmd.Action.Order)}
		panic("OrderNotUnderstood")
	}
	return
}

// runAgentModule is a generic command launcher for MIG modules that are
// built into the agent's binary. It handles commands timeout.
func runAgentModule(ctx Context, migCmd mig.Command) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("runCommand() -> %v", e)
		}
		ctx.Channels.Log <- mig.Log{CommandID: migCmd.ID, ActionID: migCmd.Action.ID, Desc: "leaving runCommand()"}.Debug()
	}()

	ctx.Channels.Log <- mig.Log{CommandID: migCmd.ID, ActionID: migCmd.Action.ID, Desc: fmt.Sprintf("executing command '%s'", migCmd.Action.Order)}.Debug()
	// waiter is a channel that receives a message when the timeout expires
	waiter := make(chan error, 1)
	var out bytes.Buffer

	// Command arguments must be in json format
	tmpargs, err := json.Marshal(migCmd.Action.Arguments)
	if err != nil {
		panic(err)
	}

	// stringify the arguments
	cmdArgs := fmt.Sprintf("%s", tmpargs)

	// build the command line and execute
	cmd := exec.Command(os.Args[0], "-m", strings.ToLower(migCmd.Action.Order), cmdArgs)
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		panic(err)
	}

	// launch the waiter in a separate goroutine
	go func() {
		waiter <- cmd.Wait()
	}()

	select {

	// Timeout case: command has reached timeout, kill it
	case <-time.After(MODULETIMEOUT):
		ctx.Channels.Log <- mig.Log{ActionID: migCmd.Action.ID, CommandID: migCmd.ID, Desc: "command timed out. Killing it."}.Err()

		// update the command status and send the response back
		migCmd.Status = "timeout"
		ctx.Channels.Results <- migCmd

		// kill the command
		err := cmd.Process.Kill()
		if err != nil {
			panic(err)
		}
		<-waiter // allow goroutine to exit

	// Normal exit case: command has finished before the timeout
	case err := <-waiter:

		if err != nil {
			ctx.Channels.Log <- mig.Log{ActionID: migCmd.Action.ID, CommandID: migCmd.ID, Desc: "command failed."}.Err()
			// update the command status and send the response back
			migCmd.Status = "failed"
			ctx.Channels.Results <- migCmd
			panic(err)

		} else {
			ctx.Channels.Log <- mig.Log{ActionID: migCmd.Action.ID, CommandID: migCmd.ID, Desc: "command succeeded."}
			err = json.Unmarshal(out.Bytes(), &migCmd.Results)
			if err != nil {
				panic(err)
			}
			// mark command status as successfully completed
			migCmd.Status = "succeeded"
			// send the results back to the scheduler
			ctx.Channels.Results <- migCmd
		}
	}
	return
}

// sendResults builds a message body and send the command results back to the scheduler
func sendResults(ctx Context, result mig.Command) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("sendResults() -> %v", e)
		}
		ctx.Channels.Log <- mig.Log{CommandID: result.ID, ActionID: result.Action.ID, Desc: "leaving sendResults()"}.Debug()
	}()

	ctx.Channels.Log <- mig.Log{CommandID: result.ID, ActionID: result.Action.ID, Desc: "sending command results"}
	result.AgentQueueLoc = ctx.Agent.QueueLoc
	body, err := json.Marshal(result)
	if err != nil {
		panic(err)
	}

	routingKey := fmt.Sprintf("mig.sched.%s", ctx.Agent.QueueLoc)
	err = publish(ctx, "mig", routingKey, body)
	if err != nil {
		panic(err)
	}

	return
}

// keepAliveAgent will send heartbeats messages to the scheduler at regular intervals
func keepAliveAgent(ctx Context) (err error) {
	// declare a keepalive message
	HeartBeat := mig.KeepAlive{
		Name:		ctx.Agent.Hostname,
		OS:		ctx.Agent.OS,
		Version:	version,
		QueueLoc:	ctx.Agent.QueueLoc,
		StartTime:	time.Now(),
	}

	// loop forever
	for {
		HeartBeat.HeartBeatTS = time.Now()
		body, err := json.Marshal(HeartBeat)
		if err != nil {
			desc := fmt.Sprintf("keepAliveAgent failed with error '%v'", err)
			ctx.Channels.Log <- mig.Log{Desc: desc}.Err()
		}
		desc := fmt.Sprintf("heartbeat '%s'", body)
		ctx.Channels.Log <- mig.Log{Desc: desc}.Debug()
		publish(ctx, "mig", "mig.keepalive", body)
		time.Sleep(ctx.Sleeper)
	}
	return
}

// publish is a generic function that sends messages to an AMQP exchange
func publish(ctx Context, exchange, routingKey string, body []byte) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("publish() -> %v", e)
		}
		ctx.Channels.Log <- mig.Log{Desc: "leaving publish()"}.Debug()
	}()

	msg := amqp.Publishing{
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
		ContentType:  "text/plain",
		Body:         []byte(body),
	}
	err = ctx.MQ.Chan.Publish(exchange, routingKey,
				true,  // is mandatory
				false, // is immediate
				msg)   // AMQP message
	if err != nil {
		panic(err)
	}
	desc := fmt.Sprintf("Message published to exchange '%s' with routing key '%s' and body '%s'", exchange, routingKey, msg.Body)
	ctx.Channels.Log <- mig.Log{Desc: desc}.Debug()
	return
}
