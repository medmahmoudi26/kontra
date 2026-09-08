from google.protobuf import struct_pb2 as _struct_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class BareRef(_message.Message):
    __slots__ = ()
    class MetaEntry(_message.Message):
        __slots__ = ()
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    SHA256_FIELD_NUMBER: _ClassVar[int]
    SIZE_FIELD_NUMBER: _ClassVar[int]
    META_FIELD_NUMBER: _ClassVar[int]
    sha256: str
    size: int
    meta: _containers.ScalarMap[str, str]
    def __init__(self, sha256: _Optional[str] = ..., size: _Optional[int] = ..., meta: _Optional[_Mapping[str, str]] = ...) -> None: ...

class EntryInput(_message.Message):
    __slots__ = ()
    UNITS_FIELD_NUMBER: _ClassVar[int]
    INPUT_REF_FIELD_NUMBER: _ClassVar[int]
    RETURN_REF_FIELD_NUMBER: _ClassVar[int]
    RUN_ID_FIELD_NUMBER: _ClassVar[int]
    IDEMPOTENCY_KEY_FIELD_NUMBER: _ClassVar[int]
    NODE_ID_FIELD_NUMBER: _ClassVar[int]
    EXPECTED_DIGEST_FIELD_NUMBER: _ClassVar[int]
    PARAMS_FIELD_NUMBER: _ClassVar[int]
    METHOD_FIELD_NUMBER: _ClassVar[int]
    SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    units: _containers.RepeatedCompositeFieldContainer[_struct_pb2.Value]
    input_ref: BareRef
    return_ref: bool
    run_id: str
    idempotency_key: str
    node_id: str
    expected_digest: str
    params: _struct_pb2.Struct
    method: str
    session_id: str
    def __init__(self, units: _Optional[_Iterable[_Union[_struct_pb2.Value, _Mapping]]] = ..., input_ref: _Optional[_Union[BareRef, _Mapping]] = ..., return_ref: _Optional[bool] = ..., run_id: _Optional[str] = ..., idempotency_key: _Optional[str] = ..., node_id: _Optional[str] = ..., expected_digest: _Optional[str] = ..., params: _Optional[_Union[_struct_pb2.Struct, _Mapping]] = ..., method: _Optional[str] = ..., session_id: _Optional[str] = ...) -> None: ...
