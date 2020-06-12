# State Encoding and Decoding

The state package implements the encoding and decoding of sentry data structures
for `go_stateify`.

Here's an overview of how encoding and decoding works.

## Encoding

Encoding produces a `statefile`, which contains a list chunks of the form
`(header, payload)`. The payload can either be some raw data, or more commonly
an encoded protobuf message representing a sentry data structure. All data
structures are encoded as an `Object` message defined in `object.proto`, with
the `value` variants encoding type information.

Encoding of an object graph begins with `state.Serialize`. Encoding happens in
two stages:

### 1. Memory Map

To discover relationships between potentially interdependent data structures
(for example, a struct may contain pointers to members of other data
structures), the encoder first walks the object graph and constructs a memory
map of the objects in the input graph.

The encoder starts at the root object and recursively visits all reachable
objects, recording the address ranges containing the underlying data for each
object. This is stored as a segment set (`addrSet`), mapping address ranges to
the of the object occupying the range; see `encodeState.values`.

Additionally, the encoder assigns each object a unique identifier which is used
to indicate relationships between objects in the statefile; see `objectID` in
`encode.go` and the `Ref` message in `object.proto`.

The entry point for this stage is the `encodeState.walkObject` call on the root
object in `encodeState.Serialize`.

### 2. Encoding and Serialization

With a full address map, the actual encoding is simple:

```
queue.push(rootObject)

while !queue.empty():
  item <- queue.pop()
  protoMsg <- encodeObject(item)
  writeObject(protoMsg)

encodeObject(obj):
  for referencedObjs referenced by obj:
    queue.push(referencedObjs)
  return protobuf representing obj
```

The encoding queue is seeded with the root object, and as references to other
objects are discovered while encoding the root object, they're added to the
queue with the `queue` function.

During `encodeObject`, the memory map is used to resolve references to other
objects. When `encodeObject` encounters a pointer, the `objectID` assigned to
the pointer target is retrieved by consulting the memory map. A pointer may
point to some data inside another data structure, in which case an accessor path
is constructed by searching for the field inside the parent object; see `lookup`
and `traverse`. This accessor path is encoded in the object stream as a list of
`Dot` messages.

The assigned `objectID`s aren't explicitly encoded in the statefile. The order
of object messages in the stream determine their IDs.

Saveable structs can have user-defined `Save` and `Load` functions, and these
are called during this stage.

### Example

Given the following data structure definitions:

```go
type system struct {
    o *outer
    i *inner
}

type outer struct {
    a  int64
    cn *container
}

type container struct {
    n    uint64
    elem *inner
}

type inner struct {
    c    container
    x, y uint64
}
```

Initialized like this:

```go
o := outer{
    a: 10,
    cn: nil,
}
i := inner{
    x: 20,
    y: 30,
    c: container{},
}
s := system{
    o: &o,
    i: &i,
}

o.cn = &i.c
o.cn.elem = &i

```

Encoding will produce an object stream like this:

```
g0r1 = struct{
     i: g0r3,
     o: g0r2,
}
g0r2 = struct{
     a: 10,
     cn: g0r3.c,
}
g0r3 = struct{
     c: struct{
             elem: g0r3,
             n: 0u,
     },
     x: 20u,
     y: 30u,
}
```

Note how `g0r3.c` is correctly encoded as the underlying `container` object for
`inner.c`, and how the pointer from `outer.cn` points to it, despite `system.i`
being discovered after the pointer to it in `system.o.cn`. Also note that
decoding isn't strictly reliant on the order of encoded object stream, as long
as the relationship between objects are correctly encoded.

## Decoding

Decoding reads the statefile and reconstructs the object graph. Decoding begins
in `decodeState.Deserialize`. Decoding is performed in a single pass over the
object stream in the statefile.

Decoding is relatively straight forward. For most primitive values, the decoder
constructs an appropriate object and fills it with the values encoded in the
statefile. Pointers need special handling, as they must point to a value
allocated elsewhere. When values are constructed, the decoder indexes them by
their `objectID`s in `decodeState.objectsByID`. The target of pointers are
resolved by searching for the target in this index by their `objectID`; see
`decodeState.register`. For pointers to values inside another value (fields in a
pointer, elements of an array), the decoder uses the accessor path to walk to
the appropriate location; see `walkChild`.

## Caveats

### Map Values

Map values are handled specially throughout encoding. For the purposes of
encoding, there are three categories of types:

1.  Concrete types which are distinct, value objects, such as primitives,
    structs by value, etc.

2.  Reference types which point to concrete types, such as pointers, slices
    (points to underlying array).

3.  Maps.

Maps are special because internally they are pointers to an opaque data
structure so the memory for a map value is actually a pointer. However many
pointer operations are not valid on map values. For example:

```go
someMap := make(map[key]value)
v := reflect.ValueOf(someMap)
v.Elem()
```

is not valid.

Generally, many code paths that handle pointers also handle maps, but with a
layer of indirection removed for the value being processed. See the various
branches on `v.Kind() == reflect.Map` throughout the encoder.

## Known Limitations

-   Only map values are supported. Encoding will intentionally fail if the input
    object graph contains a map pointer because the decoder currently can't
    handle map pointers.
