from google.protobuf import struct_pb2 as _struct_pb2
from kontra.v1 import entry_pb2 as _entry_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ActorRunInput(_message.Message):
    __slots__ = ()
    UNITS_FIELD_NUMBER: _ClassVar[int]
    UNITS_REF_FIELD_NUMBER: _ClassVar[int]
    PARAMS_FIELD_NUMBER: _ClassVar[int]
    RUN_ID_FIELD_NUMBER: _ClassVar[int]
    TENANT_FIELD_NUMBER: _ClassVar[int]
    IDEMPOTENCY_KEY_FIELD_NUMBER: _ClassVar[int]
    EXPECTED_DIGEST_FIELD_NUMBER: _ClassVar[int]
    RETURN_REF_FIELD_NUMBER: _ClassVar[int]
    units: _containers.RepeatedCompositeFieldContainer[_struct_pb2.Value]
    units_ref: _entry_pb2.BareRef
    params: _struct_pb2.Struct
    run_id: str
    tenant: str
    idempotency_key: str
    expected_digest: str
    return_ref: bool
    def __init__(self, units: _Optional[_Iterable[_Union[_struct_pb2.Value, _Mapping]]] = ..., units_ref: _Optional[_Union[_entry_pb2.BareRef, _Mapping]] = ..., params: _Optional[_Union[_struct_pb2.Struct, _Mapping]] = ..., run_id: _Optional[str] = ..., tenant: _Optional[str] = ..., idempotency_key: _Optional[str] = ..., expected_digest: _Optional[str] = ..., return_ref: _Optional[bool] = ...) -> None: ...

class ActorRunResult(_message.Message):
    __slots__ = ()
    RESULTS_FIELD_NUMBER: _ClassVar[int]
    FAILURES_FIELD_NUMBER: _ClassVar[int]
    RESULT_REF_FIELD_NUMBER: _ClassVar[int]
    results: _containers.RepeatedCompositeFieldContainer[_struct_pb2.Value]
    failures: _containers.RepeatedCompositeFieldContainer[PerUnitFailure]
    result_ref: _entry_pb2.BareRef
    def __init__(self, results: _Optional[_Iterable[_Union[_struct_pb2.Value, _Mapping]]] = ..., failures: _Optional[_Iterable[_Union[PerUnitFailure, _Mapping]]] = ..., result_ref: _Optional[_Union[_entry_pb2.BareRef, _Mapping]] = ...) -> None: ...

class PerUnitFailure(_message.Message):
    __slots__ = ()
    UNIT_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    CATEGORY_FIELD_NUMBER: _ClassVar[int]
    unit: _struct_pb2.Value
    error: ErrorInfo
    category: str
    def __init__(self, unit: _Optional[_Union[_struct_pb2.Value, _Mapping]] = ..., error: _Optional[_Union[ErrorInfo, _Mapping]] = ..., category: _Optional[str] = ...) -> None: ...

class ErrorInfo(_message.Message):
    __slots__ = ()
    TYPE_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    type: str
    message: str
    def __init__(self, type: _Optional[str] = ..., message: _Optional[str] = ...) -> None: ...
