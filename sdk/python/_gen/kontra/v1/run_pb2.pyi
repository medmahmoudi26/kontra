from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class RunEnvelope(_message.Message):
    __slots__ = ()
    ACTOR_FIELD_NUMBER: _ClassVar[int]
    RUN_ID_FIELD_NUMBER: _ClassVar[int]
    NODE_ID_FIELD_NUMBER: _ClassVar[int]
    TENANT_FIELD_NUMBER: _ClassVar[int]
    IDEMPOTENCY_KEY_FIELD_NUMBER: _ClassVar[int]
    PAYLOAD_FIELD_NUMBER: _ClassVar[int]
    PARAMS_FIELD_NUMBER: _ClassVar[int]
    DISPATCH_FIELD_NUMBER: _ClassVar[int]
    actor: Actor
    run_id: str
    node_id: str
    tenant: str
    idempotency_key: str
    payload: bytes
    params: bytes
    dispatch: DispatchOptions
    def __init__(self, actor: _Optional[_Union[Actor, _Mapping]] = ..., run_id: _Optional[str] = ..., node_id: _Optional[str] = ..., tenant: _Optional[str] = ..., idempotency_key: _Optional[str] = ..., payload: _Optional[bytes] = ..., params: _Optional[bytes] = ..., dispatch: _Optional[_Union[DispatchOptions, _Mapping]] = ...) -> None: ...

class Actor(_message.Message):
    __slots__ = ()
    NAME_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    name: str
    version: str
    def __init__(self, name: _Optional[str] = ..., version: _Optional[str] = ...) -> None: ...

class DispatchOptions(_message.Message):
    __slots__ = ()
    CHUNK_SIZE_FIELD_NUMBER: _ClassVar[int]
    PARALLEL_SESSIONS_FIELD_NUMBER: _ClassVar[int]
    SCHEDULE_TO_CLOSE_MS_FIELD_NUMBER: _ClassVar[int]
    chunk_size: int
    parallel_sessions: int
    schedule_to_close_ms: int
    def __init__(self, chunk_size: _Optional[int] = ..., parallel_sessions: _Optional[int] = ..., schedule_to_close_ms: _Optional[int] = ...) -> None: ...
