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
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"math"
	"reflect"
	"testing"
)

// TestCase is used to define a single success/failure testcase of
// serialization of a set of objects.
type TestCase struct {
	// Name is the name of the test case.
	Name string

	// Objects is the list of values to serialize.
	Objects []interface{}

	// Fail is whether the test case is supposed to fail or not.
	Fail bool
}

// runTest runs all testcases.
func runTest(t *testing.T, tests []TestCase) {
	for _, test := range tests {
		for i, root := range test.Objects {
			t.Run(fmt.Sprintf("%s.%d", test.Name, i), func(t *testing.T) {

				// Save the passed object.
				saveBuffer := &bytes.Buffer{}
				saveObjectPtr := reflect.New(reflect.TypeOf(root))
				saveObjectPtr.Elem().Set(reflect.ValueOf(root))
				if err := Save(context.Background(), saveBuffer, saveObjectPtr.Interface(), nil); err != nil {
					if test.Fail {
						// Save failed, but this case was expected to fail.
						return
					}
					t.Fatalf("Save failed unexpectedly: %v", err)
				}

				// Dump the serialized proto to aid with debugging.
				var ppBuf bytes.Buffer
				PrettyPrint(&ppBuf, bytes.NewReader(saveBuffer.Bytes()), false)
				t.Logf("Encoded state:\n%s", ppBuf.String())

				// Load a new copy of the object.
				loadObjectPtr := reflect.New(reflect.TypeOf(root))
				if err := Load(context.Background(), bytes.NewReader(saveBuffer.Bytes()), loadObjectPtr.Interface(), nil); err != nil {
					if test.Fail {
						// Load failed, but this case was expected to fail.
						return
					}
					t.Fatalf("Load failed unexpectedly: %v", err)
				}

				// Compare the values.
				loadedValue := loadObjectPtr.Elem().Interface()
				if eq := reflect.DeepEqual(root, loadedValue); !eq {
					if test.Fail {
						// Objects are different, but we expect this case to fail.
						return
					}
					t.Fatalf("Objects differs; got %#v", loadedValue)
				}

				// Everything went okay. Is that good?
				if test.Fail {
					t.Fatalf("This test was expected to fail, but didn't.")
				}
			})
		}
	}
}

// dumbStruct is a struct which does not implement the loader/saver interface.
// We expect that serialization of this struct will fail.
type dumbStruct struct {
	A int
	B int
}

// smartStruct is a struct which does implement the loader/saver interface.
// We expect that serialization of this struct will succeed.
type smartStruct struct {
	A int
	B int
}

func (s *smartStruct) StateSave(m Map) {
	m.Save("A", &s.A)
	m.Save("B", &s.B)
}

func (s *smartStruct) StateLoad(m Map) {
	m.Load("A", &s.A)
	m.Load("B", &s.B)
}

// valueLoadStruct uses a value load.
type valueLoadStruct struct {
	v int
}

func (v *valueLoadStruct) StateSave(m Map) {
	m.SaveValue("v", v.v)
}

func (v *valueLoadStruct) StateLoad(m Map) {
	m.LoadValue("v", new(int), func(value interface{}) {
		v.v = value.(int)
	})
}

// afterLoadStruct has an AfterLoad function.
type afterLoadStruct struct {
	v int
}

func (a *afterLoadStruct) StateSave(m Map) {
}

func (a *afterLoadStruct) StateLoad(m Map) {
	m.AfterLoad(func() {
		a.v++
	})
}

// genericContainer is a generic dispatcher.
type genericContainer struct {
	v interface{}
}

func (g *genericContainer) StateSave(m Map) {
	m.Save("v", &g.v)
}

func (g *genericContainer) StateLoad(m Map) {
	m.Load("v", &g.v)
}

// sliceContainer is a generic slice.
type sliceContainer struct {
	v []interface{}
}

func (s *sliceContainer) StateSave(m Map) {
	m.Save("v", &s.v)
}

func (s *sliceContainer) StateLoad(m Map) {
	m.Load("v", &s.v)
}

// mapContainer is a generic map.
type mapContainer struct {
	v map[int]interface{}
}

func (mc *mapContainer) StateSave(m Map) {
	m.Save("v", &mc.v)
}

func (mc *mapContainer) StateLoad(m Map) {
	// Some of the test cases below assume legacy behavior wherein maps
	// will automatically inherit dependencies.
	m.LoadWait("v", &mc.v)
}

type mapPtrContainer struct {
	v *map[int]interface{}
}

func (mc *mapPtrContainer) StateSave(m Map) {
	m.Save("v", &mc.v)
}

func (mc *mapPtrContainer) StateLoad(m Map) {
	// Some of the test cases below assume legacy behavior wherein maps
	// will automatically inherit dependencies.
	m.LoadWait("v", &mc.v)
}

// dumbMap is a map which does not implement the loader/saver interface.
// Serialization of this map will default to the standard encode/decode logic.
type dumbMap map[string]int

// pointerStruct contains various pointers, shared and non-shared, and pointers
// to pointers. We expect that serialization will respect the structure.
type pointerStruct struct {
	A *int
	B *int
	C *int
	D *int

	AA **int
	BB **int
}

func (p *pointerStruct) StateSave(m Map) {
	m.Save("A", &p.A)
	m.Save("B", &p.B)
	m.Save("C", &p.C)
	m.Save("D", &p.D)
	m.Save("AA", &p.AA)
	m.Save("BB", &p.BB)
}

func (p *pointerStruct) StateLoad(m Map) {
	m.Load("A", &p.A)
	m.Load("B", &p.B)
	m.Load("C", &p.C)
	m.Load("D", &p.D)
	m.Load("AA", &p.AA)
	m.Load("BB", &p.BB)
}

// testInterface is a trivial interface example.
type testInterface interface {
	Foo()
}

// testImpl is a trivial implementation of testInterface.
type testImpl struct {
}

// Foo satisfies testInterface.
func (t *testImpl) Foo() {
}

// testImpl is trivially serializable.
func (t *testImpl) StateSave(m Map) {
}

// testImpl is trivially serializable.
func (t *testImpl) StateLoad(m Map) {
}

// testI demonstrates interface dispatching.
type testI struct {
	I testInterface
}

func (t *testI) StateSave(m Map) {
	m.Save("I", &t.I)
}

func (t *testI) StateLoad(m Map) {
	m.Load("I", &t.I)
}

// cycleStruct is used to implement basic cycles.
type cycleStruct struct {
	c *cycleStruct
}

func (c *cycleStruct) StateSave(m Map) {
	m.Save("c", &c.c)
}

func (c *cycleStruct) StateLoad(m Map) {
	m.Load("c", &c.c)
}

// badCycleStruct actually has deadlocking dependencies.
//
// This should pass if b.b = {nil|b} and fail otherwise.
type badCycleStruct struct {
	b *badCycleStruct
}

func (b *badCycleStruct) StateSave(m Map) {
	m.Save("b", &b.b)
}

func (b *badCycleStruct) StateLoad(m Map) {
	m.LoadWait("b", &b.b)
	m.AfterLoad(func() {
		// This is not executable, since AfterLoad requires that the
		// object and all dependencies are complete. This should cause
		// a deadlock error during load.
	})
}

// emptyStructPointer points to an empty struct.
type emptyStructPointer struct {
	nothing *struct{}
}

func (e *emptyStructPointer) StateSave(m Map) {
	m.Save("nothing", &e.nothing)
}

func (e *emptyStructPointer) StateLoad(m Map) {
	m.Load("nothing", &e.nothing)
}

// truncateInteger truncates an integer.
type truncateInteger struct {
	v  int64
	v2 int32
}

func (t *truncateInteger) StateSave(m Map) {
	t.v2 = int32(t.v)
	m.Save("v", &t.v)
}

func (t *truncateInteger) StateLoad(m Map) {
	m.Load("v", &t.v2)
	t.v = int64(t.v2)
}

// truncateUnsignedInteger truncates an unsigned integer.
type truncateUnsignedInteger struct {
	v  uint64
	v2 uint32
}

func (t *truncateUnsignedInteger) StateSave(m Map) {
	t.v2 = uint32(t.v)
	m.Save("v", &t.v)
}

func (t *truncateUnsignedInteger) StateLoad(m Map) {
	m.Load("v", &t.v2)
	t.v = uint64(t.v2)
}

// truncateFloat truncates a floating point number.
type truncateFloat struct {
	v  float64
	v2 float32
}

func (t *truncateFloat) StateSave(m Map) {
	t.v2 = float32(t.v)
	m.Save("v", &t.v)
}

func (t *truncateFloat) StateLoad(m Map) {
	m.Load("v", &t.v2)
	t.v = float64(t.v2)
}

type outer struct {
	a  int64
	cn *container
}

func (o *outer) StateSave(m Map) {
	m.Save("a", &o.a)
	m.Save("cn", &o.cn)
}

func (o *outer) StateLoad(m Map) {
	m.Load("a", &o.a)
	m.LoadValue("cn", new(*container), func(x interface{}) {
		o.cn = x.(*container)
	})
}

type container struct {
	n    uint64
	elem *inner
}

func (c *container) init(o *outer, i *inner) {
	c.elem = i
	o.cn = c
}

func (c *container) StateSave(m Map) {
	m.Save("n", &c.n)
	m.Save("elem", &c.elem)
}

func (c *container) StateLoad(m Map) {
	m.Load("n", &c.n)
	m.Load("elem", &c.elem)
}

type inner struct {
	c    container
	x, y uint64
}

func (i *inner) StateSave(m Map) {
	m.Save("c", &i.c)
	m.Save("x", &i.x)
	m.Save("y", &i.y)
}

func (i *inner) StateLoad(m Map) {
	m.Load("c", &i.c)
	m.Load("x", &i.x)
	m.Load("y", &i.y)
}

type system struct {
	o *outer
	i *inner
}

func (s *system) StateSave(m Map) {
	m.Save("o", &s.o)
	m.Save("i", &s.i)
}

func (s *system) StateLoad(m Map) {
	m.Load("o", &s.o)
	m.Load("i", &s.i)
}

func TestTypes(t *testing.T) {
	// x and y are basic integers, while xp points to x.
	x := 1
	y := 2
	xp := &x

	// cs is a single object cycle.
	cs := cycleStruct{nil}
	cs.c = &cs

	// cs1 and cs2 are in a two object cycle.
	cs1 := cycleStruct{nil}
	cs2 := cycleStruct{nil}
	cs1.c = &cs2
	cs2.c = &cs1

	// bs is a single object cycle.
	bs := badCycleStruct{nil}
	bs.b = &bs

	// bs2 and bs2 are in a deadlocking cycle.
	bs1 := badCycleStruct{nil}
	bs2 := badCycleStruct{nil}
	bs1.b = &bs2
	bs2.b = &bs1

	// regular nils.
	var (
		nilmap   dumbMap
		nilslice []byte
	)

	// embed points to embedded fields.
	embed1 := pointerStruct{}
	embed1.AA = &embed1.A
	embed2 := pointerStruct{}
	embed2.BB = &embed2.B

	// es1 contains two structs pointing to the same empty struct.
	es := emptyStructPointer{new(struct{})}
	es1 := []emptyStructPointer{es, es}

	o := outer{
		a: 10,
	}
	i := inner{
		x: 20,
		y: 30,
	}
	i.c.init(&o, &i)

	s := system{
		o: &o,
		i: &i,
	}

	tests := []TestCase{
		{
			Name: "interlocking field pointer",
			Objects: []interface{}{
				s,
			},
		},
		{
			Name: "bool",
			Objects: []interface{}{
				true,
				false,
			},
		},
		{
			Name: "integers",
			Objects: []interface{}{
				int(0),
				int(1),
				int(-1),
				int8(0),
				int8(1),
				int8(-1),
				int16(0),
				int16(1),
				int16(-1),
				int32(0),
				int32(1),
				int32(-1),
				int64(0),
				int64(1),
				int64(-1),
			},
		},
		{
			Name: "unsigned integers",
			Objects: []interface{}{
				uint(0),
				uint(1),
				uint8(0),
				uint8(1),
				uint16(0),
				uint16(1),
				uint32(1),
				uint64(0),
				uint64(1),
			},
		},
		{
			Name: "strings",
			Objects: []interface{}{
				"",
				"foo",
				"bar",
				"\xa0",
			},
		},
		{
			Name: "slices",
			Objects: []interface{}{
				[]int{-1, 0, 1},
				[]*int{&x, &x, &x},
				[]int{1, 2, 3}[0:1],
				[]int{1, 2, 3}[1:2],
				make([]byte, 32),
				make([]byte, 32)[:16],
				make([]byte, 32)[:16:20],
				nilslice,
			},
		},
		{
			Name: "arrays",
			Objects: []interface{}{
				&[5]bool{false, true, false, true},
				&[5]uint8{0, 1, 2, 3},
				&[5]byte{0, 1, 2, 3},
				&[5]uint16{0, 1, 2, 3},
				&[5]uint{0, 1, 2, 3},
				&[5]uint32{0, 1, 2, 3},
				&[5]uint64{0, 1, 2, 3},
				&[5]uintptr{0, 1, 2, 3},
				&[5]int8{0, -1, -2, -3},
				&[5]int16{0, -1, -2, -3},
				&[5]int32{0, -1, -2, -3},
				&[5]int64{0, -1, -2, -3},
				&[5]float32{0, 1.1, 2.2, 3.3},
				&[5]float64{0, 1.1, 2.2, 3.3},
			},
		},
		{
			Name: "pointers",
			Objects: []interface{}{
				&pointerStruct{A: &x, B: &x, C: &y, D: &y, AA: &xp, BB: &xp},
				&pointerStruct{},
			},
		},
		{
			Name: "empty struct",
			Objects: []interface{}{
				struct{}{},
			},
		},
		{
			Name: "unenlightened structs",
			Objects: []interface{}{
				&dumbStruct{A: 1, B: 2},
			},
			Fail: true,
		},
		{
			Name: "enlightened structs",
			Objects: []interface{}{
				&smartStruct{A: 1, B: 2},
			},
		},
		{
			Name: "load-hooks",
			Objects: []interface{}{
				&afterLoadStruct{v: 1},
				&valueLoadStruct{v: 1},
				&genericContainer{v: &afterLoadStruct{v: 1}},
				&genericContainer{v: &valueLoadStruct{v: 1}},
				&sliceContainer{v: []interface{}{&afterLoadStruct{v: 1}}},
				&sliceContainer{v: []interface{}{&valueLoadStruct{v: 1}}},
				&mapContainer{v: map[int]interface{}{0: &afterLoadStruct{v: 1}}},
				&mapContainer{v: map[int]interface{}{0: &valueLoadStruct{v: 1}}},
			},
		},
		{
			Name: "maps",
			Objects: []interface{}{
				dumbMap{"a": -1, "b": 0, "c": 1},
				map[smartStruct]int{{}: 0, {A: 1}: 1},
				nilmap,
				&mapContainer{v: map[int]interface{}{0: &smartStruct{A: 1}}},
			},
		},
		{
			Name: "map ptr",
			Objects: []interface{}{
				&mapPtrContainer{v: &map[int]interface{}{0: &smartStruct{A: 1}}},
			},
			Fail: true,
		},
		{
			Name: "interfaces",
			Objects: []interface{}{
				&testI{&testImpl{}},
				&testI{nil},
				&testI{(*testImpl)(nil)},
			},
		},
		{
			Name: "unregistered-interfaces",
			Objects: []interface{}{
				&genericContainer{v: afterLoadStruct{v: 1}},
				&genericContainer{v: valueLoadStruct{v: 1}},
				&sliceContainer{v: []interface{}{afterLoadStruct{v: 1}}},
				&sliceContainer{v: []interface{}{valueLoadStruct{v: 1}}},
				&mapContainer{v: map[int]interface{}{0: afterLoadStruct{v: 1}}},
				&mapContainer{v: map[int]interface{}{0: valueLoadStruct{v: 1}}},
			},
			Fail: true,
		},
		{
			Name: "cycles",
			Objects: []interface{}{
				&cs,
				&cs1,
				&cycleStruct{&cs1},
				&cycleStruct{&cs},
				&badCycleStruct{nil},
				&bs,
			},
		},
		{
			Name: "deadlock",
			Objects: []interface{}{
				&bs1,
			},
			Fail: true,
		},
		{
			Name: "embed",
			Objects: []interface{}{
				&embed1,
				&embed2,
			},
		},
		{
			Name: "empty structs",
			Objects: []interface{}{
				new(struct{}),
				es,
				es1,
			},
		},
		{
			Name: "truncated okay",
			Objects: []interface{}{
				&truncateInteger{v: 1},
				&truncateUnsignedInteger{v: 1},
				&truncateFloat{v: 1.0},
			},
		},
		{
			Name: "truncated bad",
			Objects: []interface{}{
				&truncateInteger{v: math.MaxInt32 + 1},
				&truncateUnsignedInteger{v: math.MaxUint32 + 1},
				&truncateFloat{v: math.MaxFloat32 * 2},
			},
			Fail: true,
		},
	}

	runTest(t, tests)
}

// benchStruct is used for benchmarking.
type benchStruct struct {
	b *benchStruct

	// Dummy data is included to ensure that these objects are large.
	// This is to detect possible regression when registering objects.
	_ [4096]byte
}

func (b *benchStruct) StateSave(m Map) {
	m.Save("b", &b.b)
}

func (b *benchStruct) StateLoad(m Map) {
	m.LoadWait("b", &b.b)
	m.AfterLoad(b.afterLoad)
}

func (b *benchStruct) afterLoad() {
	// Do nothing, just force scheduling.
}

// buildObject builds a benchmark object.
func buildObject(n int) (b *benchStruct) {
	for i := 0; i < n; i++ {
		b = &benchStruct{b: b}
	}
	return
}

func BenchmarkEncoding(b *testing.B) {
	b.StopTimer()
	bs := buildObject(b.N)
	var stats Stats
	b.StartTimer()
	if err := Save(context.Background(), ioutil.Discard, bs, &stats); err != nil {
		b.Errorf("save failed: %v", err)
	}
	b.StopTimer()
	if b.N > 1000 {
		b.Logf("breakdown (n=%d): %s", b.N, &stats)
	}
}

func BenchmarkDecoding(b *testing.B) {
	b.StopTimer()
	bs := buildObject(b.N)
	var newBS benchStruct
	buf := &bytes.Buffer{}
	if err := Save(context.Background(), buf, bs, nil); err != nil {
		b.Errorf("save failed: %v", err)
	}
	var stats Stats
	b.StartTimer()
	if err := Load(context.Background(), buf, &newBS, &stats); err != nil {
		b.Errorf("load failed: %v", err)
	}
	b.StopTimer()
	if b.N > 1000 {
		b.Logf("breakdown (n=%d): %s", b.N, &stats)
	}
}

func init() {
	Register("stateTest.system", (*system)(nil))
	Register("stateTest.outer", (*outer)(nil))
	Register("stateTest.container", (*container)(nil))
	Register("stateTest.inner", (*inner)(nil))
	Register("stateTest.smartStruct", (*smartStruct)(nil))
	Register("stateTest.afterLoadStruct", (*afterLoadStruct)(nil))
	Register("stateTest.valueLoadStruct", (*valueLoadStruct)(nil))
	Register("stateTest.genericContainer", (*genericContainer)(nil))
	Register("stateTest.sliceContainer", (*sliceContainer)(nil))
	Register("stateTest.mapContainer", (*mapContainer)(nil))
	Register("stateTest.mapPtrContainer", (*mapPtrContainer)(nil))
	Register("stateTest.pointerStruct", (*pointerStruct)(nil))
	Register("stateTest.testImpl", (*testImpl)(nil))
	Register("stateTest.testI", (*testI)(nil))
	Register("stateTest.cycleStruct", (*cycleStruct)(nil))
	Register("stateTest.badCycleStruct", (*badCycleStruct)(nil))
	Register("stateTest.emptyStructPointer", (*emptyStructPointer)(nil))
	Register("stateTest.truncateInteger", (*truncateInteger)(nil))
	Register("stateTest.truncateUnsignedInteger", (*truncateUnsignedInteger)(nil))
	Register("stateTest.truncateFloat", (*truncateFloat)(nil))
	Register("stateTest.benchStruct", (*benchStruct)(nil))
}
