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

package state

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"reflect"
	"sort"

	"github.com/golang/protobuf/proto"
	pb "gvisor.dev/gvisor/pkg/state/object_go_proto"
)

// objectRef is the type and identity of an object occupying a memory address
// range. This is the value type for addrSet.
//
// This object is also queued for encoding. The id field is only assigned when
// the object is queued. All references should be resolved via queue before
// using the id field.
type objectRef struct {
	id  objectID
	obj reflect.Value
	objectRefEntry
}

// encodeState is state used for encoding.
//
// The encoding process is a breadth-first traversal of the object graph. The
// inherent races and dependencies are much simpler than the decode case.
type encodeState struct {
	// ctx is the encode context.
	ctx context.Context

	// lastID is the last allocated object ID.
	lastID objectID

	// values tracks the address ranges occupied by objects, along with the
	// types of these objects. This is used to locate pointer targets, including
	// pointers to fields within another type.
	//
	// Multiple objects may overlap in memory iff the larger object fully
	// contains the smaller one, and the type of the smaller object matches a
	// field or array element's type at the appropriate offset. An arbitrary
	// number of objects may be nested in this manner.
	//
	// Note that this does not track zero-sized objects, those are tracked
	// by zeroValues below.
	values addrSet

	// zeroValues tracks zero-sized objects.
	zeroValues map[reflect.Type]*objectRef

	// w is the output stream.
	w io.Writer

	// pending is the list of objects to be serialized.
	//
	// Note that this may be modified during walkObject, as container
	// objects are discovered.
	pending objectRefList

	// stats is the passed stats object.
	stats *Stats
}

// isSameSizeParent returns true if child is a field value or element within
// parent. Only a struct or array can have a child value.
//
// isSameSizeParent deals with objects like this:
//
// struct child {
//     // fields..
// }
//
// struct parent {
//     c child
// }
//
// var p parent
// record(&p.c)
//
// Here, &p and &p.c occupy the exact same address range.
//
// Or like this:
//
// struct child {
//     // fields
// }
//
// var arr [1]parent
// record(&arr[0])
//
// Similarly, &arr[0] and &arr[0].c have the exact same address range.
//
// Precondition: parent and child must occupy the same memory.
func isSameSizeParent(parent reflect.Value, childType reflect.Type) bool {
	switch parent.Kind() {
	case reflect.Struct:
		for i := 0; i < parent.NumField(); i++ {
			field := parent.Field(i)
			if field.Type() == childType {
				return true
			}
			// Recurse through any intermediate types.
			if isSameSizeParent(field, childType) {
				return true
			}
			// Does it make sense to keep going if the first field
			// doesn't match? Yes, because there might be an
			// arbitrary number of zero-sized fields before we get
			// a match, and childType itself can be zero-sized.
		}
		return false
	case reflect.Array:
		// The only case where an array with more than one elements can
		// return true is if childType is zero-sized. In such cases,
		// it's ambiguous which element contains the match since a
		// zero-sized child object fully fits in any of the zero-sized
		// elements in an array... However since all elements are of
		// the same type, we only need to check one element.
		//
		// For non-zero-sized childTypes, parent.Len() must be 1, but a
		// combination of the precondition and an implicit comparison
		// between the array element size and childType ensures this.
		return parent.Len() > 0 && isSameSizeParent(parent.Index(0), childType)
	default:
		return false
	}
}

// record records the address range occupied by an object. Returns true iff
// object occupies the same memory as another object and needs to be walked.
func (es *encodeState) record(obj reflect.Value) bool {
	addr := obj.Pointer()

	// Is this a map pointer? Just record the single address. It is not
	// possible to take any pointers into the map internals.
	if obj.Kind() == reflect.Map {
		r := addrRange{addr, addr + 8}
		if !es.values.IsEmptyRange(r) {
			// Ensure the map types match.
			existing := es.values.LowerBoundSegment(addr).Value()
			if existing.obj.Type() != obj.Type() {
				panic(fmt.Errorf("overlapping map objects at %#v: [new object] %#v [existing object type] %s", r, obj, existing.obj))
			}
			return false // Need not rescan.
		}
		es.values.Add(r, &objectRef{
			obj: obj,
		})
		return true // Must scan.
	}

	// If not a map, then the object must be a pointer.
	if obj.Kind() != reflect.Ptr {
		panic(fmt.Errorf("attempt to record non-map and non-pointer object %#v", obj))
	}

	obj = obj.Elem() // Value from here.

	// Is this a zero-sized type?
	typ := obj.Type()
	size := typ.Size()
	if size == 0 {
		// All zero-sized objects point to a dummy byte within the
		// runtime. There's no sense recording this in the address map.
		// We add this to the dedicated zeroValues.
		if _, ok := es.zeroValues[typ]; !ok {
			es.zeroValues[typ] = &objectRef{
				obj: obj,
			}
		}
		return false // Nothing to scan.
	}

	// Calculate the container.
	end := addr + size
	r := addrRange{addr, end}
	if !es.values.IsEmptyRange(r) { // Something already registered in the address range.
		seg := es.values.LowerBoundSegment(addr)
		existing := seg.Value()
		switch {
		case (seg.Start() < addr && seg.End() >= end) || (seg.Start() <= addr && seg.End() > end):
			// The previously reigstered object is larger than
			// this, no need to rescan.
			return false
		case seg.Start() == addr && seg.End() == end:
			// This is exactly the same as a previously registered
			// object. Either the type is a composite type with one
			// element (one or the other), or this is just the
			// object being registered again.
			if typ == existing.obj.Type() {
				// Same object already.
			} else if isSameSizeParent(obj, existing.obj.Type()) {
				// Update to the proper type.
				existing.obj = obj
			}
			return false // Same size: no need to rescan.
		case (seg.Start() > addr && seg.End() <= end) || (seg.Start() >= addr && seg.End() < end):
			// The previously registered object is smaller than
			// this. Grow the segment, update and rescan.
			es.values.Remove(seg)
			es.values.Add(r, &objectRef{
				obj: obj,
			})
			return true // Scan again.
		default:
			// There is a non-sensical overlap.
			panic(fmt.Errorf("overlapping objects: [new object] %#v [existing object] %#v", obj, existing.obj))
		}
	}

	// The only remaining case is a pointer value that doesn't overlap with
	// any registered addresses. Create a new entry for it.
	es.values.Add(r, &objectRef{
		obj: obj,
	})
	return true // Requires scan.
}

// queue adds an object to the serialization queue.
//
// Precondition: obj must have been previously recorded by encodeState.record.
func (es *encodeState) queue(obj reflect.Value) *pb.Ref {
	es.walkObject(obj)

	// Lookup the object.
	ref, dots := es.lookup(obj)

	// If we have a local copy of the object that can be manipulated (was
	// not obtained through reflection exclusively), then we may need to
	// replace the objectRef object.
	if !ref.obj.CanSet() {
		if obj.Kind() == reflect.Map && ref.obj.Type() == obj.Type() {
			ref.obj = obj
		} else if ref.obj.Type() == obj.Elem().Type() {
			ref.obj = obj.Elem()
		}
	}

	// Assign an ID as we push to pending.
	if ref.id == 0 {
		es.lastID++
		ref.id = es.lastID
		es.pending.PushBack(ref)
	}

	return &pb.Ref{
		Value: uint64(ref.id),
		Dot:   dots,
	}
}

// traverse searches target object within a root object, where the target
// object is a struct field or array element within root, with potentially
// multiple intervening types. traverse records the set of field or element
// traversals required to reach the target in ref.Dot.
//
// The decoder uses this information to resolve pointers to child objects
// within the parent. See walkChild in decode.go.
//
// Precondition: traverse may only be called on pointer objects. The target
// object must lie completely within [rootAddr, rootAddr + sizeof(rootType).
func traverse(rootType, targetType reflect.Type, rootAddr, targetAddr uintptr) ([]*pb.Dot, bool) {
	dots, ok := traverseInverse(rootType, targetType, rootAddr, targetAddr)
	if !ok {
		return nil, false
	}
	// Reverse the returned slice.
	for i, j := 0, len(dots)-1; i < j; i, j = i+1, j-1 {
		dots[i], dots[j] = dots[j], dots[i]
	}
	return dots, true
}

// traverseInverse is the implementation for traverse.
//
// This returns the dot slice in reverse order.
func traverseInverse(rootType, targetType reflect.Type, rootAddr, targetAddr uintptr) ([]*pb.Dot, bool) {
	// Technically, the start address check below isn't necessary because
	// lookup() already checked that target fits within root's address
	// range.  Thus, if rootType == target.Type(), it's impossible to have
	// another object of the same type fit within root's address range.
	// However, we can provided a more meaningful error message if we have
	// root's start address available.
	if targetType == rootType && targetAddr == rootAddr {
		// The two objects have the same type and start at the same
		// address, so we're done.
		return nil, true
	}

	switch rootType.Kind() {
	case reflect.Struct:
		cursor := 0
		for i := 0; i < rootType.NumField(); i++ {
			field := rootType.Field(i)
			dots, ok := traverse(field.Type, targetType, rootAddr+uintptr(cursor), targetAddr)
			if ok {
				return append(dots, &pb.Dot{
					Value: &pb.Dot_Field{rootType.Field(i).Name},
				}), true
			}
			cursor += int(field.Type.Size())
		}
		// None of the fields matched.
		return nil, false
	case reflect.Array:
		if rootType.Len() == 0 {
			panic(fmt.Errorf("requested traversal on empty array %#v", rootType.Name()))
		}
		// Since arrays have homogenous types, all elements have the
		// same size and we can compute where the target lives.
		elemSize := int(rootType.Elem().Size())
		n := int(targetAddr-rootAddr) / elemSize // Relies on integer division rounding down.
		if rootType.Len() < n {
			panic(fmt.Errorf("traversal target addr %#v is beyond the end of the array type %v @ addr %#v with %v elements", targetAddr, rootType, rootAddr, rootType.Len()))
		}
		dots, ok := traverse(rootType.Elem(), targetType, rootAddr+uintptr(n*elemSize), targetAddr)
		if ok {
			return append(dots, &pb.Dot{
				Value: &pb.Dot_Index{uint32(n)},
			}), true
		}
		// The array didn't match.
		return nil, false
	default:
		// For any other type, there's no possibility of aliasing so if
		// the types didn't match earlier then we have an addresss
		// collision which shouldn't be possible at this point.
		return nil, false
	}
}

// lookup retrieves the serialization reference for an object previously queued
// via encodeState.queue.
//
// Precondition: obj must have been previously queued, lookup panics
// otherwise.
func (es *encodeState) lookup(obj reflect.Value) (*objectRef, []*pb.Dot) {
	addr := obj.Pointer()
	if obj.Kind() == reflect.Map {
		// Leave alone.
	} else if obj.Kind() == reflect.Ptr {
		obj = obj.Elem() // Switch to the underlying value.
	} else {
		panic(fmt.Errorf("looking up non-map and non-pointer object %#v", obj))
	}
	typ := obj.Type()
	size := typ.Size()

	// Handle zero-sized types.
	if size == 0 {
		objRef, ok := es.zeroValues[typ]
		if !ok {
			panic(fmt.Errorf("attempted to lookup unregistered zero-sized object %#v", obj.Interface()))
		}
		return objRef, nil
	}

	// Lookup in the value set.
	ar := addrRange{addr, addr + size}
	seg, _ := es.values.Find(addr)

	// Was obj previously registered?
	if !seg.Ok() {
		panic(fmt.Errorf("attempted to lookup unregistered heap object %#v at %#v", obj.Interface(), ar))
	}

	// Sanity check: seg must contain all of obj.
	if !seg.Range().IsSupersetOf(ar) {
		panic(fmt.Errorf("obj %#v @ %#v doesn't fully reside in the registered address range %#v",
			obj.Interface(), ar, seg.Range()))
	}

	// Now construct the traversal rule.
	objRef := seg.Value()
	startAddr := seg.Range().Start
	dots, ok := traverse(objRef.obj.Type(), typ, startAddr, addr)
	if !ok {
		panic(fmt.Errorf("attempted to locate an inner field or elem of type %#v at addr %#v, inside %#v which occupies addr range %#v",
			obj, obj.Pointer(), objRef.obj, seg.Range()))
	}

	return objRef, dots
}

// encodeMap encodes a map.
func (es *encodeState) encodeMap(obj reflect.Value) *pb.Map {
	var (
		keys   []*pb.Object
		values []*pb.Object
	)
	for _, k := range obj.MapKeys() {
		v := obj.MapIndex(k)
		kp := es.encodeObject(k, false)
		vp := es.encodeObject(v, false)
		keys = append(keys, kp)
		values = append(values, vp)
	}
	return &pb.Map{Keys: keys, Values: values}
}

// encodeStruct encodes a composite object.
func (es *encodeState) encodeStruct(obj reflect.Value) *pb.Struct {
	// Invoke the save.
	m := Map{newInternalMap(es, nil, nil)}
	defer internalMapPool.Put(m.internalMap)
	if !obj.CanAddr() {
		// Force it to a * type of the above; this involves a copy.
		localObj := reflect.New(obj.Type())
		localObj.Elem().Set(obj)
		obj = localObj.Elem()
	}
	fns, ok := obj.Addr().Interface().(SaveLoader)
	if ok {
		// Invoke the provided saver.
		fns.StateSave(m)
	} else if obj.NumField() == 0 {
		// Allow unregistered anonymous, empty structs.
		return &pb.Struct{}
	} else {
		// Propagate an error.
		panic(fmt.Errorf("unregistered type %T", obj.Interface()))
	}

	// Sort the underlying slice, and check for duplicates. This is done
	// once instead of on each add, because performing this sort once is
	// far more efficient.
	if len(m.data) > 1 {
		sort.Slice(m.data, func(i, j int) bool {
			return m.data[i].name < m.data[j].name
		})
		for i := range m.data {
			if i > 0 && m.data[i-1].name == m.data[i].name {
				panic(fmt.Errorf("duplicate name %s", m.data[i].name))
			}
		}
	}

	// Encode the resulting fields.
	fields := make([]*pb.Field, 0, len(m.data))
	for _, e := range m.data {
		fields = append(fields, &pb.Field{
			Name:  e.name,
			Value: e.object,
		})
	}

	// Return the encoded object.
	return &pb.Struct{Fields: fields}
}

// encodeArray encodes an array.
func (es *encodeState) encodeArray(obj reflect.Value) *pb.Array {
	var (
		contents []*pb.Object
	)
	for i := 0; i < obj.Len(); i++ {
		entry := es.encodeObject(obj.Index(i), false)
		contents = append(contents, entry)
	}
	return &pb.Array{Contents: contents}
}

// encodeInterface encodes an interface.
//
// Precondition: the value is not nil.
func (es *encodeState) encodeInterface(obj reflect.Value) *pb.Interface {
	// Check for the nil interface.
	obj = reflect.ValueOf(obj.Interface())
	if !obj.IsValid() {
		return &pb.Interface{
			Type:  "", // left alone in decode.
			Value: &pb.Object{Value: &pb.Object_NilValue{}},
		}
	}
	// We have an interface value here. How do we save that? We
	// resolve the underlying type and save it as a dispatchable.
	typName, ok := registeredTypes.lookupName(obj.Type())
	if !ok {
		panic(fmt.Errorf("type %s is not registered", obj.Type()))
	}

	// Encode the object again.
	return &pb.Interface{
		Type:  typName,
		Value: es.encodeObject(obj, false),
	}
}

func isScalarKind(k reflect.Kind) bool {
	switch k {
	case reflect.Array, reflect.Slice, reflect.Ptr, reflect.Map, reflect.Struct, reflect.Interface:
		return false
	default:
		return true
	}
}

// walkObject walks an object and records types and references.
func (es *encodeState) walkObject(obj reflect.Value) {
	for {
		switch obj.Kind() {
		case reflect.Bool:
			return
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			return
		case reflect.Float32, reflect.Float64:
			return
		case reflect.Chan, reflect.Func, reflect.UnsafePointer:
			return
		case reflect.Array:
			if isScalarKind(obj.Type().Elem().Kind()) {
				// No need to walk the array, the elements
				// aren't composite types that can potentially
				// contain other types. Stopping the walk early
				// here can be a significant optimization for
				// large arrays (for example, very large byte
				// buffers).
				return
			}
			for i := 0; i < obj.Len(); i++ {
				es.walkObject(obj.Index(i))
			}
			return // Completed.
		case reflect.Slice:
			// This constructs a pointer to an array.
			obj = arrayFromSlice(obj)
		case reflect.String:
			// Technically strings may point to the same underlying
			// bytes, but given the bytes must be immutable by
			// language constraints, we don't bother with this.
			return
		case reflect.Ptr:
			// Walk the pointer (if not registered).
			if !obj.IsNil() && es.record(obj) {
				obj = obj.Elem()
			} else {
				return // Nil or done.
			}
		case reflect.Interface:
			if !obj.IsNil() {
				obj = obj.Elem()
			} else {
				return // Nil or done.
			}
		case reflect.Struct:
			// Walk all fields. Note that we optimize for
			// structures with a single field and don't recurse,
			// because this is a common pattern.
			if obj.NumField() == 0 {
				return // Nothing.
			}
			for i := 0; i < obj.NumField()-1; i++ {
				es.walkObject(obj.Field(i))
			}
			obj = obj.Field(obj.NumField() - 1) // Last field.
		case reflect.Map:
			if !obj.IsNil() && es.record(obj) {
				// Similar to arrays, it can be a large
				// optimization to stop the walk early if map
				// key or value types are scalars.
				isKeyScalar := isScalarKind(obj.Type().Key().Kind())
				isValScalar := isScalarKind(obj.Type().Elem().Kind())
				iter := obj.MapRange()
				for iter.Next() {
					if !isKeyScalar {
						es.walkObject(iter.Key())
					}
					if !isValScalar {
						es.walkObject(iter.Value())
					}
				}
			}
			return // Completed (recursed above).
		default:
			panic(fmt.Errorf("unknown primitive %#v", obj))
		}
	}
}

// encodeObject encodes an object.
//
// If mapAsValue is true, then a map will be encoded directly.
func (es *encodeState) encodeObject(obj reflect.Value, mapAsValue bool) (object *pb.Object) {
	es.stats.Start(obj)
	defer es.stats.Done()

	switch obj.Kind() {
	case reflect.Bool:
		object = &pb.Object{Value: &pb.Object_BoolValue{obj.Bool()}}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		object = &pb.Object{Value: &pb.Object_Int64Value{obj.Int()}}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		object = &pb.Object{Value: &pb.Object_Uint64Value{obj.Uint()}}
	case reflect.Float32, reflect.Float64:
		object = &pb.Object{Value: &pb.Object_DoubleValue{obj.Float()}}
	case reflect.Array:
		switch obj.Type().Elem().Kind() {
		case reflect.Uint8:
			object = &pb.Object{Value: &pb.Object_ByteArrayValue{pbSlice(obj).Interface().([]byte)}}
		case reflect.Uint16:
			// 16-bit slices are serialized as 32-bit slices.
			// See object.proto for details.
			s := pbSlice(obj).Interface().([]uint16)
			t := make([]uint32, len(s))
			for i := range s {
				t[i] = uint32(s[i])
			}
			object = &pb.Object{Value: &pb.Object_Uint16ArrayValue{&pb.Uint16S{Values: t}}}
		case reflect.Uint32:
			object = &pb.Object{Value: &pb.Object_Uint32ArrayValue{&pb.Uint32S{Values: pbSlice(obj).Interface().([]uint32)}}}
		case reflect.Uint64:
			object = &pb.Object{Value: &pb.Object_Uint64ArrayValue{&pb.Uint64S{Values: pbSlice(obj).Interface().([]uint64)}}}
		case reflect.Uintptr:
			object = &pb.Object{Value: &pb.Object_UintptrArrayValue{&pb.Uintptrs{Values: pbSlice(obj).Interface().([]uint64)}}}
		case reflect.Int8:
			object = &pb.Object{Value: &pb.Object_Int8ArrayValue{&pb.Int8S{Values: pbSlice(obj).Interface().([]byte)}}}
		case reflect.Int16:
			// 16-bit slices are serialized as 32-bit slices.
			// See object.proto for details.
			s := pbSlice(obj).Interface().([]int16)
			t := make([]int32, len(s))
			for i := range s {
				t[i] = int32(s[i])
			}
			object = &pb.Object{Value: &pb.Object_Int16ArrayValue{&pb.Int16S{Values: t}}}
		case reflect.Int32:
			object = &pb.Object{Value: &pb.Object_Int32ArrayValue{&pb.Int32S{Values: pbSlice(obj).Interface().([]int32)}}}
		case reflect.Int64:
			object = &pb.Object{Value: &pb.Object_Int64ArrayValue{&pb.Int64S{Values: pbSlice(obj).Interface().([]int64)}}}
		case reflect.Bool:
			object = &pb.Object{Value: &pb.Object_BoolArrayValue{&pb.Bools{Values: pbSlice(obj).Interface().([]bool)}}}
		case reflect.Float32:
			object = &pb.Object{Value: &pb.Object_Float32ArrayValue{&pb.Float32S{Values: pbSlice(obj).Interface().([]float32)}}}
		case reflect.Float64:
			object = &pb.Object{Value: &pb.Object_Float64ArrayValue{&pb.Float64S{Values: pbSlice(obj).Interface().([]float64)}}}
		default:
			object = &pb.Object{Value: &pb.Object_ArrayValue{es.encodeArray(obj)}}
		}
	case reflect.Slice:
		if obj.IsNil() || obj.Cap() == 0 {
			// Handled specially in decode; store as nil value.
			object = &pb.Object{Value: &pb.Object_NilValue{}}
		} else {
			// Serialize a slice as the array plus length and capacity.
			object = &pb.Object{Value: &pb.Object_SliceValue{&pb.Slice{
				Capacity: uint32(obj.Cap()),
				Length:   uint32(obj.Len()),
				RefValue: es.queue(arrayFromSlice(obj)),
			}}}
		}
	case reflect.String:
		object = &pb.Object{Value: &pb.Object_StringValue{[]byte(obj.String())}}
	case reflect.Ptr:
		if obj.IsNil() {
			// Handled specially in decode; store as a nil value.
			object = &pb.Object{Value: &pb.Object_NilValue{}}
		} else {
			object = &pb.Object{Value: &pb.Object_RefValue{es.queue(obj)}}
		}
	case reflect.Interface:
		// We don't check for IsNil here, as we want to encode type
		// information. The case of the empty interface (no type, no
		// value) is handled by encodeInteface.
		object = &pb.Object{Value: &pb.Object_InterfaceValue{es.encodeInterface(obj)}}
	case reflect.Struct:
		object = &pb.Object{Value: &pb.Object_StructValue{es.encodeStruct(obj)}}
	case reflect.Map:
		if obj.IsNil() {
			// Handled specially in decode; store as a nil value.
			object = &pb.Object{Value: &pb.Object_NilValue{}}
		} else if mapAsValue {
			// Encode the map directly.
			object = &pb.Object{Value: &pb.Object_MapValue{es.encodeMap(obj)}}
		} else {
			// Encode a reference to the map.
			object = &pb.Object{Value: &pb.Object_RefValue{es.queue(obj)}}
		}
	default:
		panic(fmt.Errorf("unknown primitive %#v", obj.Interface()))
	}

	return
}

// Serialize serializes the object state.
//
// This function may panic and should be run in safely().
func (es *encodeState) Serialize(obj reflect.Value) {
	// First, walk all reachable objects and record their memory addresses.
	// This will update the address map, from which we can compute fields
	// or elements when serializing references.
	es.queue(obj.Addr())

	if es.pending.Len() == 0 {
		panic(fmt.Errorf("pending is empty"))
	}

	// Serialize all pending objects; this list may grow as we serialize.
	for id := objectID(1); es.pending.Len() > 0; id++ {
		qo := es.pending.Front()
		if qo.id != id {
			panic(fmt.Errorf("expected id %d, got %d", id, qo.id))
		}
		o := es.encodeObject(qo.obj, true)
		if err := es.writeObject(o); err != nil {
			panic(err)
		}
		es.pending.Remove(qo)
	}

	// Write a zero-length terminal at the end; this is a sanity check
	// applied at decode time as well (see decode.go).
	if err := WriteHeader(es.w, 0, false); err != nil {
		panic(err)
	}
}

// WriteHeader writes a header.
//
// Each object written to the statefile should be prefixed with a header. In
// order to generate statefiles that play nicely with debugging tools, raw
// writes should be prefixed with a header with object set to false and the
// appropriate length. This will allow tools to skip these regions.
func WriteHeader(w io.Writer, length uint64, object bool) error {
	// The lowest-order bit encodes whether this is a valid object. This is
	// a purely internal convention, but allows the object flag to be
	// returned from ReadHeader.
	length = length << 1
	if object {
		length |= 0x1
	}

	// Write a header.
	var hdr [32]byte
	encodedLen := binary.PutUvarint(hdr[:], length)
	for done := 0; done < encodedLen; {
		n, err := w.Write(hdr[done:encodedLen])
		done += n
		if n == 0 && err != nil {
			return err
		}
	}

	return nil
}

// writeObject writes an object to the stream.
func (es *encodeState) writeObject(obj *pb.Object) error {
	// Marshal the proto.
	buf, err := proto.Marshal(obj)
	if err != nil {
		return err
	}

	// Write the object header.
	if err := WriteHeader(es.w, uint64(len(buf)), true); err != nil {
		return err
	}

	// Write the object.
	for done := 0; done < len(buf); {
		n, err := es.w.Write(buf[done:])
		done += n
		if n == 0 && err != nil {
			return err
		}
	}

	return nil
}

// addrSetFunctions is used by addrSet.
type addrSetFunctions struct{}

func (addrSetFunctions) MinKey() uintptr {
	return 0
}

func (addrSetFunctions) MaxKey() uintptr {
	return ^uintptr(0)
}

func (addrSetFunctions) ClearValue(val **objectRef) {
	*val = nil
}

func (addrSetFunctions) Merge(_ addrRange, val1 *objectRef, _ addrRange, val2 *objectRef) (*objectRef, bool) {
	if val1.obj != val2.obj {
		return val1, false
	}
	if val1.id != 0 || val2.id != 0 {
		panic(fmt.Errorf("merging objects with non-zero ids: %#v and %#v", val1.obj, val2.obj))
	}
	return val1, true
}

func (addrSetFunctions) Split(_ addrRange, val *objectRef, _ uintptr) (*objectRef, *objectRef) {
	return val, val
}
