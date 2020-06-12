// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package state provides functionality related to saving and loading object
// graphs.  For most types, it provides a set of default saving / loading logic
// that will be invoked automatically if custom logic is not defined.
//
//     Kind             Support
//     ----             -------
//     Bool             default
//     Int              default
//     Int8             default
//     Int16            default
//     Int32            default
//     Int64            default
//     Uint             default
//     Uint8            default
//     Uint16           default
//     Uint32           default
//     Uint64           default
//     Float32          default
//     Float64          default
//     Complex64        custom
//     Complex128       custom
//     Array            default
//     Chan             custom
//     Func             custom
//     Interface        custom
//     Map              default (*)
//     Ptr              default
//     Slice            default
//     String           default
//     Struct           custom
//     UnsafePointer    custom
//
// (*) Maps are treated as value types by this package, even if they are
// pointers internally. Maps can only be saved as values, map pointers are
// currently not supported.
//
// See README.md for an overview of how encoding and decoding works.
package state

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"runtime"

	pb "gvisor.dev/gvisor/pkg/state/object_go_proto"
)

// objectID is a unique identifier assigned to each object to be serialized.
// Each instance of an object is considered separately, i.e. if there are two
// objects of the same type in the object graph being serialized, they'll be
// assigned unique objectIDs.
type objectID uint64

// ErrState is returned when an error is encountered during encode/decode.
type ErrState struct {
	// err is the underlying error.
	err error

	// trace is the stack trace.
	trace string
}

// Error returns a sensible description of the state error.
func (e *ErrState) Error() string {
	return fmt.Sprintf("%v:\n%s", e.err, e.trace)
}

// UnwrapErrState returns the underlying error in ErrState.
//
// If err is not *ErrState, err is returned directly.
func UnwrapErrState(err error) error {
	if e, ok := err.(*ErrState); ok {
		return e.err
	}
	return err
}

// Save saves the given object state.
func Save(ctx context.Context, w io.Writer, rootPtr interface{}, stats *Stats) error {
	// Create the encoding state.
	es := &encodeState{
		ctx:        ctx,
		w:          w,
		stats:      stats,
		zeroValues: make(map[reflect.Type]*objectRef),
	}

	// Perform the encoding.
	return safely(func() {
		es.Serialize(reflect.ValueOf(rootPtr).Elem())
	})
}

// Load loads a checkpoint.
func Load(ctx context.Context, r io.Reader, rootPtr interface{}, stats *Stats) error {
	// Create the decoding state.
	ds := &decodeState{
		ctx:         ctx,
		objectsByID: make(map[objectID]*objectState),
		deferred:    make(map[objectID]*pb.Object),
		r:           r,
		stats:       stats,
	}

	// Attempt our decode.
	return safely(func() {
		ds.Deserialize(reflect.ValueOf(rootPtr).Elem())
	})
}

// SaveLoader is an interface for state functions.
type SaveLoader interface {
	// StateSave saves the state of the object to the given Map.
	StateSave(m Map)

	// StateLoad loads the state of the object from the given Map.
	StateLoad(m Map)
}

// validateType validates that the type implements SaveLoader.
func validateType(instance interface{}) {
	if _, ok := instance.(SaveLoader); !ok {
		panic(fmt.Errorf("type %T does not implement SaveLoader", instance))
	}
}

type typeDatabase struct {
	// nameToType is a forward lookup table.
	nameToType map[string]reflect.Type

	// typeToName is the reverse lookup table.
	typeToName map[reflect.Type]string
}

// registeredTypes is a database used for SaveInterface and LoadInterface.
var registeredTypes = typeDatabase{
	nameToType: make(map[string]reflect.Type),
	typeToName: make(map[reflect.Type]string),
}

// register registers a type under the given name. This will generally be
// called via init() methods, and therefore uses panic to propagate errors.
func (t *typeDatabase) register(name string, typ reflect.Type) {
	// We can't allow name collisions.
	if ot, ok := t.nameToType[name]; ok {
		panic(fmt.Errorf("type %q can't use name %q, already in use by type %q", typ.Name(), name, ot.Name()))
	}

	// Or multiple registrations.
	if on, ok := t.typeToName[typ]; ok {
		panic(fmt.Errorf("type %q can't be registered as %q, already registered as %q", typ.Name(), name, on))
	}

	t.nameToType[name] = typ
	t.typeToName[typ] = name
}

// lookupType finds a type given a name.
func (t *typeDatabase) lookupType(name string) (reflect.Type, bool) {
	typ, ok := t.nameToType[name]
	return typ, ok
}

// lookupName finds a name given a type.
func (t *typeDatabase) lookupName(typ reflect.Type) (string, bool) {
	name, ok := t.typeToName[typ]
	return name, ok
}

// Register must be called for any interface implementation types that
// implements Loader.
//
// Register should be called either immediately after startup or via init()
// methods. Double registration of either names or types will result in a panic.
//
// No synchronization is provided; this should only be called in init.
//
// Example usage:
//
// 	state.Register("Foo", (*Foo)(nil))
//
func Register(name string, instance interface{}) {
	validateType(instance)
	registeredTypes.register(name, reflect.TypeOf(instance))
}

// IsZeroValue checks if the given value is the zero value.
//
// This function is used by the stateify tool.
func IsZeroValue(val interface{}) bool {
	return val == nil || reflect.ValueOf(val).Elem().IsZero()
}

// safely executes the given function, catching a panic and unpacking as an error.
//
// The error flow through the state package uses panic and recover. There are
// two important reasons for this:
//
// 1) Many of the reflection methods will already panic with invalid data or
// violated assumptions. We would want to recover anyways here.
//
// 2) It allows us to eliminate boilerplate within Save() and Load() functions.
// In nearly all cases, when the low-level serialization functions fail, you
// will want the checkpoint to fail anyways. Plumbing errors through every
// method doesn't add a lot of value. If there are specific error conditions
// that you'd like to handle, you should add appropriate functionality to
// objects themselves prior to calling Save() and Load().
func safely(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			es := new(ErrState)
			if e, ok := r.(error); ok {
				es.err = e
			} else {
				es.err = fmt.Errorf("%v", r)
			}

			// Make a stack. We don't know how big it will be ahead
			// of time, but want to make sure we get the whole
			// thing. So we just do a stupid brute force approach.
			var stack []byte
			for sz := 1024; ; sz *= 2 {
				stack = make([]byte, sz)
				n := runtime.Stack(stack, false)
				if n < sz {
					es.trace = string(stack[:n])
					break
				}
			}

			// Set the error.
			err = es
		}
	}()

	// Execute the function.
	fn()
	return nil
}
