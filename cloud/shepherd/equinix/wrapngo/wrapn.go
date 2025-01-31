// Package wrapngo wraps packngo methods providing the following usability
// enhancements:
// - API call rate limiting
// - resource-aware call retries
// - use of a configurable back-off algorithm implementation
// - context awareness
//
// The implementation is provided with the following caveats:
//
// There can be only one call in flight. Concurrent calls to API-related
// methods of the same client will block. Calls returning packngo structs will
// return nil data when a non-nil error value is returned. An
// os.ErrDeadlineExceeded will be returned after the underlying API calls time
// out beyond the chosen back-off algorithm implementation's maximum allowed
// retry interval. Other errors, excluding context.Canceled and
// context.DeadlineExceeded, indicate either an error originating at Equinix'
// API endpoint (which may still stem from invalid call inputs), or a network
// error.
//
// Packngo wrappers included below may return timeout errors even after the
// wrapped calls succeed in the event server reply could not have been
// received.
//
// This implies that effects of mutating calls can't always be verified
// atomically, requiring explicit synchronization between API users, regardless
// of the retry/recovery logic used.
//
// Having that in mind, some call wrappers exposed by this package will attempt
// to recover from this kind of situations by requesting information on any
// resources created, and retrying the call if needed. This approach assumes
// any concurrent mutating API users will be synchronized, as it should be in
// any case.
//
// Another way of handling this problem would be to leave it up to the user to
// retry calls if needed, though this would leak Equinix Metal API, and
// complicate implementations depending on this package. Due to that, the prior
// approach was chosen.
package wrapngo

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
	"github.com/packethost/packngo"
)

// Opts conveys configurable Client parameters.
type Opts struct {
	// User and APIKey are the credentials used to authenticate with
	// Metal API.

	User   string
	APIKey string

	// Optional parameters:

	// BackOff controls the client's behavior in the event of API calls failing
	// due to IO timeouts by adjusting the lower bound on time taken between
	// subsequent calls.
	BackOff func() backoff.BackOff

	// APIRate is the minimum time taken between subsequent API calls.
	APIRate time.Duration

	// Parallelism defines how many calls to the Equinix API will be issued in
	// parallel. When this limit is reached, subsequent attmepts to call the API will
	// block. The order of serving of pending calls is currently undefined.
	//
	// If not defined (ie. 0), defaults to 1.
	Parallelism int
}

func (o *Opts) RegisterFlags() {
	flag.StringVar(&o.User, "equinix_api_username", "", "Username for Equinix API")
	flag.StringVar(&o.APIKey, "equinix_api_key", "", "Key/token/password for Equinix API")
	flag.IntVar(&o.Parallelism, "equinix_parallelism", 1, "How many parallel connections to the Equinix API will be allowed")
}

// Client is a limited interface of methods that the Shepherd uses on Equinix. It
// is provided to allow for dependency injection of a fake equinix API for tests.
type Client interface {
	// GetDevice wraps packngo's cl.Devices.Get.
	GetDevice(ctx context.Context, pid, did string) (*packngo.Device, error)
	// ListDevices wraps packngo's cl.Device.List.
	ListDevices(ctx context.Context, pid string) ([]packngo.Device, error)
	// CreateDevice attempts to create a new device according to the provided
	// request. The request _must_ configure a HardwareReservationID. This call
	// attempts to be as idempotent as possible, and will return ErrRaceLost if a
	// retry was needed but in the meantime the requested hardware reservation from
	// which this machine was requested got lost.
	CreateDevice(ctx context.Context, request *packngo.DeviceCreateRequest) (*packngo.Device, error)

	// ListReservations returns a complete list of hardware reservations associated
	// with project pid. This is an expensive method that takes a while to execute,
	// handle with care.
	ListReservations(ctx context.Context, pid string) ([]packngo.HardwareReservation, error)

	// ListSSHKeys wraps packngo's cl.Keys.List.
	ListSSHKeys(ctx context.Context) ([]packngo.SSHKey, error)
	// CreateSSHKey is idempotent - the key label can be used only once. Further
	// calls referring to the same label and key will not yield errors. See the
	// package comment for more info on this method's behavior and returned error
	// values.
	CreateSSHKey(ctx context.Context, req *packngo.SSHKeyCreateRequest) (*packngo.SSHKey, error)
	// UpdateSSHKey is idempotent - values included in r can be applied only once,
	// while subsequent updates using the same data don't produce errors. See the
	// package comment for information on this method's behavior and returned error
	// values.
	UpdateSSHKey(ctx context.Context, kid string, req *packngo.SSHKeyUpdateRequest) (*packngo.SSHKey, error)

	Close()
}

// client implements the Client interface.
type client struct {
	username string
	token    string
	o        *Opts
	rlt      *time.Ticker

	// serializer is a N-semaphore channel (configured by opts.Parallelism) which is
	// used to limit the number of concurrent calls to the Equinix API.
	serializer chan (struct{})
}

// New creates a Client instance based on Opts. PACKNGO_DEBUG environment
// variable can be set prior to the below call to enable verbose packngo
// debug logs.
func New(opts *Opts) Client {
	return new(opts)
}

func new(opts *Opts) *client {
	// Apply the defaults.
	if opts.APIRate == 0 {
		opts.APIRate = 2 * time.Second
	}
	if opts.BackOff == nil {
		opts.BackOff = func() backoff.BackOff {
			return backoff.NewExponentialBackOff()
		}
	}
	if opts.Parallelism == 0 {
		opts.Parallelism = 1
	}

	return &client{
		username: opts.User,
		token:    opts.APIKey,
		o:        opts,
		rlt:      time.NewTicker(opts.APIRate),

		serializer: make(chan struct{}, opts.Parallelism),
	}
}

func (c *client) Close() {
	c.rlt.Stop()
}

var (
	ErrRaceLost              = errors.New("race lost with another API user")
	ErrNoReservationProvided = errors.New("hardware reservation must be set")
)

func (e *client) CreateDevice(ctx context.Context, r *packngo.DeviceCreateRequest) (*packngo.Device, error) {
	if r.HardwareReservationID == "" {
		return nil, ErrNoReservationProvided
	}
	// Add a tag to the request to detect if someone snatches a hardware reservation
	// from under us.
	witnessTag := fmt.Sprintf("wrapngo-idempotency-%s", uuid.New().String())
	r.Tags = append(r.Tags, witnessTag)

	return wrap(ctx, e, func(cl *packngo.Client) (*packngo.Device, error) {
		//Does the device already exist?
		res, _, err := cl.HardwareReservations.Get(r.HardwareReservationID, nil)
		if err != nil {
			return nil, fmt.Errorf("couldn't check if device already exists: %w", err)
		}
		if res == nil {
			return nil, fmt.Errorf("unexpected nil response")
		}
		if res.Device != nil {
			// Check if we lost the race for this hardware reservation.
			tags := make(map[string]bool)
			for _, tag := range res.Device.Tags {
				tags[tag] = true
			}
			if !tags[witnessTag] {
				return nil, ErrRaceLost
			}
			return res.Device, nil
		}

		// No device yet. Try to create it.
		dev, _, err := cl.Devices.Create(r)
		if err == nil {
			return dev, nil
		}
		// In case of a transient failure (eg. network issue), we retry the whole
		// operation, which means we first check again if the device already exists. If
		// it's a permanent error from the API, the backoff logic will fail immediately.
		return nil, fmt.Errorf("couldn't create device: %w", err)
	})
}

func (e *client) ListDevices(ctx context.Context, pid string) ([]packngo.Device, error) {
	return wrap(ctx, e, func(cl *packngo.Client) ([]packngo.Device, error) {
		res, _, err := cl.Devices.List(pid, nil)
		return res, err
	})
}

func (e *client) GetDevice(ctx context.Context, pid, did string) (*packngo.Device, error) {
	return wrap(ctx, e, func(cl *packngo.Client) (*packngo.Device, error) {
		d, _, err := cl.Devices.Get(did, nil)
		return d, err
	})
}

// Currently unexported, only used in tests.
func (e *client) deleteDevice(ctx context.Context, did string) error {
	_, err := wrap(ctx, e, func(cl *packngo.Client) (*struct{}, error) {
		_, err := cl.Devices.Delete(did, false)
		if httpStatusCode(err) == http.StatusNotFound {
			// 404s may pop up as an after effect of running the back-off
			// algorithm, and as such should not be propagated.
			return nil, nil
		}
		return nil, err
	})
	return err
}

func (e *client) ListReservations(ctx context.Context, pid string) ([]packngo.HardwareReservation, error) {
	return wrap(ctx, e, func(cl *packngo.Client) ([]packngo.HardwareReservation, error) {
		res, _, err := cl.HardwareReservations.List(pid, nil)
		return res, err
	})
}

func (e *client) CreateSSHKey(ctx context.Context, r *packngo.SSHKeyCreateRequest) (*packngo.SSHKey, error) {
	return wrap(ctx, e, func(cl *packngo.Client) (*packngo.SSHKey, error) {
		// Does the key already exist?
		ks, _, err := cl.SSHKeys.List()
		if err != nil {
			return nil, fmt.Errorf("SSHKeys.List: %w", err)
		}
		for _, k := range ks {
			if k.Label == r.Label {
				if k.Key != r.Key {
					return nil, fmt.Errorf("key label already in use for a different key")
				}
				return &k, nil
			}
		}

		// No key yet. Try to create it.
		k, _, err := cl.SSHKeys.Create(r)
		if err != nil {
			return nil, fmt.Errorf("SSHKeys.Create: %w", err)
		}
		return k, nil
	})
}

func (e *client) UpdateSSHKey(ctx context.Context, id string, r *packngo.SSHKeyUpdateRequest) (*packngo.SSHKey, error) {
	return wrap(ctx, e, func(cl *packngo.Client) (*packngo.SSHKey, error) {
		k, _, err := cl.SSHKeys.Update(id, r)
		if err != nil {
			return nil, fmt.Errorf("SSHKeys.Update: %w", err)
		}
		return k, err
	})
}

// Currently unexported, only used in tests.
func (e *client) deleteSSHKey(ctx context.Context, id string) error {
	_, err := wrap(ctx, e, func(cl *packngo.Client) (struct{}, error) {
		_, err := cl.SSHKeys.Delete(id)
		if err != nil {
			return struct{}{}, fmt.Errorf("SSHKeys.Delete: %w", err)
		}
		return struct{}{}, err
	})
	return err
}

func (e *client) ListSSHKeys(ctx context.Context) ([]packngo.SSHKey, error) {
	return wrap(ctx, e, func(cl *packngo.Client) ([]packngo.SSHKey, error) {
		ks, _, err := cl.SSHKeys.List()
		if err != nil {
			return nil, fmt.Errorf("SSHKeys.List: %w", err)
		}
		return ks, nil
	})
}

// Currently unexported, only used in tests.
func (e *client) getSSHKey(ctx context.Context, id string) (*packngo.SSHKey, error) {
	return wrap(ctx, e, func(cl *packngo.Client) (*packngo.SSHKey, error) {
		k, _, err := cl.SSHKeys.Get(id, nil)
		if err != nil {
			return nil, fmt.Errorf("SSHKeys.Get: %w", err)
		}
		return k, nil
	})
}
