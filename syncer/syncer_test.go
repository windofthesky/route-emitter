package syncer_test

import (
	"os"
	"time"

	"github.com/apcera/nats"
	"github.com/cloudfoundry-incubator/receptor"
	"github.com/cloudfoundry-incubator/receptor/fake_receptor"
	"github.com/cloudfoundry-incubator/route-emitter/cfroutes"
	"github.com/cloudfoundry-incubator/route-emitter/routing_table"
	. "github.com/cloudfoundry-incubator/route-emitter/syncer"
	fake_metrics_sender "github.com/cloudfoundry/dropsonde/metric_sender/fake"
	"github.com/cloudfoundry/dropsonde/metrics"
	"github.com/cloudfoundry/gunk/diegonats"
	"github.com/pivotal-golang/clock/fakeclock"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const logGuid = "some-log-guid"

var _ = Describe("Syncer", func() {
	const (
		processGuid   = "process-guid-1"
		containerPort = 8080
		instanceGuid  = "instance-guid-1"
		lrpHost       = "1.2.3.4"
	)

	var (
		receptorClient *fake_receptor.FakeClient
		natsClient     *diegonats.FakeNATSClient
		syncer         *Syncer
		process        ifrit.Process
		syncMessages   routing_table.MessagesToEmit
		messagesToEmit routing_table.MessagesToEmit
		clock          *fakeclock.FakeClock
		clockStep      time.Duration
		syncInterval   time.Duration

		shutdown chan struct{}

		desiredResponse receptor.DesiredLRPResponse
		actualResponses []receptor.ActualLRPResponse

		routerStartMessages chan<- *nats.Msg
		fakeMetricSender    *fake_metrics_sender.FakeMetricSender
	)

	BeforeEach(func() {
		receptorClient = new(fake_receptor.FakeClient)
		natsClient = diegonats.NewFakeClient()

		clock = fakeclock.NewFakeClock(time.Now())
		clockStep = 1 * time.Second
		syncInterval = 10 * time.Second

		startMessages := make(chan *nats.Msg)
		routerStartMessages = startMessages

		natsClient.WhenSubscribing("router.start", func(callback nats.MsgHandler) error {
			go func() {
				for msg := range startMessages {
					callback(msg)
				}
			}()

			return nil
		})

		//what follows is fake data to distinguish between
		//the "sync" and "emit" codepaths
		dummyEndpoint := routing_table.Endpoint{InstanceGuid: "instance-guid-1", Host: "1.1.1.1", Port: 11, ContainerPort: 1111}
		dummyMessage := routing_table.RegistryMessageFor(dummyEndpoint, routing_table.Routes{Hostnames: []string{"foo.com", "bar.com"}, LogGuid: logGuid})
		syncMessages = routing_table.MessagesToEmit{
			RegistrationMessages: []routing_table.RegistryMessage{dummyMessage},
		}

		dummyEndpoint = routing_table.Endpoint{InstanceGuid: "instance-guid-2", Host: "2.2.2.2", Port: 22, ContainerPort: 2222}
		dummyMessage = routing_table.RegistryMessageFor(dummyEndpoint, routing_table.Routes{Hostnames: []string{"baz.com"}, LogGuid: logGuid})
		messagesToEmit = routing_table.MessagesToEmit{
			RegistrationMessages: []routing_table.RegistryMessage{dummyMessage},
		}

		desiredResponse = receptor.DesiredLRPResponse{
			ProcessGuid: processGuid,
			Ports:       []uint16{containerPort},
			Routes:      cfroutes.CFRoutes{{Hostnames: []string{"route-1", "route-2"}, Port: containerPort}}.RoutingInfo(),
			LogGuid:     logGuid,
		}

		actualResponses = []receptor.ActualLRPResponse{
			{
				ProcessGuid:  processGuid,
				InstanceGuid: instanceGuid,
				CellID:       "cell-id",
				Domain:       "domain",
				Index:        1,
				Address:      lrpHost,
				Ports: []receptor.PortMapping{
					{HostPort: 1234, ContainerPort: containerPort},
				},
				State: receptor.ActualLRPStateRunning,
			},
			{
				Index: 0,
				State: receptor.ActualLRPStateUnclaimed,
			},
		}

		receptorClient.DesiredLRPsReturns([]receptor.DesiredLRPResponse{desiredResponse}, nil)
		receptorClient.ActualLRPsReturns(actualResponses, nil)

		fakeMetricSender = fake_metrics_sender.NewFakeMetricSender()
		metrics.Initialize(fakeMetricSender)
	})

	JustBeforeEach(func() {
		logger := lagertest.NewTestLogger("test")
		syncer = NewSyncer(clock, syncInterval, natsClient, logger)

		shutdown = make(chan struct{})

		go func() {
			for {
				select {
				case <-time.After(100 * time.Millisecond):
					clock.Increment(clockStep)
				case <-shutdown:
					return
				}
			}
		}()

		process = ifrit.Invoke(syncer)
	})

	AfterEach(func() {
		process.Signal(os.Interrupt)
		Eventually(process.Wait()).Should(Receive(BeNil()))
		close(shutdown)
		close(routerStartMessages)
	})

	Describe("getting the heartbeat interval from the router", func() {
		var greetings chan *nats.Msg
		BeforeEach(func() {
			greetings = make(chan *nats.Msg, 3)
			natsClient.WhenPublishing("router.greet", func(msg *nats.Msg) error {
				greetings <- msg
				return nil
			})
		})

		Context("when the router emits a router.start", func() {
			Context("using an interval", func() {
				JustBeforeEach(func() {
					routerStartMessages <- &nats.Msg{
						Data: []byte(`{
						"minimumRegisterIntervalInSeconds":1,
						"pruneThresholdInSeconds": 3
						}`),
					}
				})

				It("should emit routes with the frequency of the passed-in-interval", func() {
					Eventually(syncer.Events().Emit, 2).Should(Receive())
					t1 := clock.Now()

					Eventually(syncer.Events().Emit, 2).Should(Receive())
					t2 := clock.Now()

					Ω(t2.Sub(t1)).Should(BeNumerically("~", 1*time.Second, 200*time.Millisecond))
				})

				It("should only greet the router once", func() {
					Eventually(greetings).Should(Receive())
					Consistently(greetings, 1).ShouldNot(Receive())
				})
			})
		})

		Context("when the router does not emit a router.start", func() {
			It("should keep greeting the router until it gets an interval", func() {
				//get the first greeting
				Eventually(greetings, 2).Should(Receive())

				//get the second greeting, and respond
				var msg *nats.Msg
				Eventually(greetings, 2).Should(Receive(&msg))
				go natsClient.Publish(msg.Reply, []byte(`{"minimumRegisterIntervalInSeconds":1, "pruneThresholdInSeconds": 3}`))

				//should no longer be greeting the router
				Consistently(greetings).ShouldNot(Receive())
			})
		})

		Context("after getting the first interval, when a second interval arrives", func() {
			JustBeforeEach(func() {
				routerStartMessages <- &nats.Msg{
					Data: []byte(`{"minimumRegisterIntervalInSeconds":1, "pruneThresholdInSeconds": 3}`),
				}
			})

			It("should modify its update rate", func() {
				routerStartMessages <- &nats.Msg{
					Data: []byte(`{"minimumRegisterIntervalInSeconds":2, "pruneThresholdInSeconds": 6}`),
				}

				//first emit should be pretty quick, it is in response to the incoming heartbeat interval
				Eventually(syncer.Events().Emit).Should(Receive())
				t1 := clock.Now()

				//subsequent emit should follow the interval
				Eventually(syncer.Events().Emit).Should(Receive())
				t2 := clock.Now()

				Ω(t2.Sub(t1)).Should(BeNumerically("~", 2*time.Second, 200*time.Millisecond))
			})
		})

		Context("if it never hears anything from a router anywhere", func() {
			It("should still be able to shutdown", func() {
				process.Signal(os.Interrupt)
				Eventually(process.Wait()).Should(Receive(BeNil()))
			})
		})
	})

	Describe("syncing", func() {
		BeforeEach(func() {
			receptorClient.ActualLRPsStub = func() ([]receptor.ActualLRPResponse, error) {
				return nil, nil
			}
			syncInterval = 500 * time.Millisecond

			clockStep = 250 * time.Millisecond
		})

		JustBeforeEach(func() {
			//we set the emit interval real high to avoid colliding with our sync interval
			routerStartMessages <- &nats.Msg{
				Data: []byte(`{"minimumRegisterIntervalInSeconds":10, "pruneThresholdInSeconds": 20}`),
			}
		})

		Context("after the router greets", func() {
			BeforeEach(func() {
				syncInterval = 10 * time.Minute
			})

			It("syncs", func() {
				Eventually(syncer.Events().Sync).Should(Receive())
			})
		})

		Context("on a specified interval", func() {
			It("should sync", func() {
				var t1 time.Time
				var t2 time.Time

				select {
				case <-syncer.Events().Sync:
					t1 = clock.Now()
				case <-time.After(500 * time.Millisecond):
					Fail("did not receive a sync event")
				}

				select {
				case <-syncer.Events().Sync:
					t2 = clock.Now()
				case <-time.After(500 * time.Millisecond):
					Fail("did not receive a sync event")
				}

				Ω(t2.Sub(t1)).Should(BeNumerically("~", syncInterval, 100*time.Millisecond))
			})
		})
	})
})
