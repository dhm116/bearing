package model

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// enums reports every enum field in m, at any depth, whose number the enum
// doesn't define (ProtoJSON accepts {"direction": 7}). Well-known types hold
// no Bearing enums and are skipped, so value trees aren't walked here.
func (c *checker) enums(path string, m proto.Message) {
	if m == nil {
		return
	}
	c.enumsIn(path, m.ProtoReflect())
}

func (c *checker) enumsIn(path string, m protoreflect.Message) {
	if !m.IsValid() || m.Descriptor().ParentFile().Package() == "google.protobuf" {
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		p := join(path, string(fd.Name()))
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() != nil {
				v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
					c.enumsIn(fmt.Sprintf("%s[%q]", p, k.String()), mv.Message())
					return true
				})
			}
		case fd.IsList():
			l := v.List()
			for i := range l.Len() {
				c.enumValue(fmt.Sprintf("%s[%d]", p, i), fd, l.Get(i))
			}
		default:
			c.enumValue(p, fd, v)
		}
		return true
	})
}

func (c *checker) enumValue(path string, fd protoreflect.FieldDescriptor, v protoreflect.Value) {
	switch {
	case fd.Enum() != nil:
		if fd.Enum().Values().ByNumber(v.Enum()) == nil {
			c.add(codeMalformed, path, "%d is not a %s value", v.Enum(), fd.Enum().Name())
		}
	case fd.Message() != nil:
		c.enumsIn(path, v.Message())
	}
}
