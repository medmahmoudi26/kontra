// @a2ui/web_core 0.15.0 and @a2ui/lit 0.15.0 (Apache-2.0) with lit 3.3.3 (BSD-3-Clause) and their dependencies: built by scripts/vendor-a2ui/build, see vendor/a2ui/README.md. Unmodified upstream code; licences in LICENSE, NOTICE and THIRD-PARTY-LICENSES beside this file.
var __defProp = Object.defineProperty;
var __export = (target, all) => {
  for (var name in all)
    __defProp(target, name, { get: all[name], enumerable: true });
};

// node_modules/zod-to-json-schema/dist/esm/Options.js
var ignoreOverride = /* @__PURE__ */ Symbol("Let zodToJsonSchema decide on which parser to use");
var defaultOptions = {
  name: void 0,
  $refStrategy: "root",
  basePath: ["#"],
  effectStrategy: "input",
  pipeStrategy: "all",
  dateStrategy: "format:date-time",
  mapStrategy: "entries",
  removeAdditionalStrategy: "passthrough",
  allowedAdditionalProperties: true,
  rejectedAdditionalProperties: false,
  definitionPath: "definitions",
  target: "jsonSchema7",
  strictUnions: false,
  definitions: {},
  errorMessages: false,
  markdownDescription: false,
  patternStrategy: "escape",
  applyRegexFlags: false,
  emailStrategy: "format:email",
  base64Strategy: "contentEncoding:base64",
  nameStrategy: "ref",
  openAiAnyTypeName: "OpenAiAnyType"
};
var getDefaultOptions = (options) => typeof options === "string" ? {
  ...defaultOptions,
  name: options
} : {
  ...defaultOptions,
  ...options
};

// node_modules/zod-to-json-schema/dist/esm/Refs.js
var getRefs = (options) => {
  const _options = getDefaultOptions(options);
  const currentPath = _options.name !== void 0 ? [..._options.basePath, _options.definitionPath, _options.name] : _options.basePath;
  return {
    ..._options,
    flags: { hasReferencedOpenAiAnyType: false },
    currentPath,
    propertyPath: void 0,
    seen: new Map(Object.entries(_options.definitions).map(([name, def]) => [
      def._def,
      {
        def: def._def,
        path: [..._options.basePath, _options.definitionPath, name],
        // Resolution of references will be forced even though seen, so it's ok that the schema is undefined here for now.
        jsonSchema: void 0
      }
    ]))
  };
};

// node_modules/zod-to-json-schema/dist/esm/errorMessages.js
function addErrorMessage(res, key, errorMessage, refs) {
  if (!refs?.errorMessages)
    return;
  if (errorMessage) {
    res.errorMessage = {
      ...res.errorMessage,
      [key]: errorMessage
    };
  }
}
function setResponseValueAndErrors(res, key, value, errorMessage, refs) {
  res[key] = value;
  addErrorMessage(res, key, errorMessage, refs);
}

// node_modules/zod-to-json-schema/dist/esm/getRelativePath.js
var getRelativePath = (pathA, pathB) => {
  let i7 = 0;
  for (; i7 < pathA.length && i7 < pathB.length; i7++) {
    if (pathA[i7] !== pathB[i7])
      break;
  }
  return [(pathA.length - i7).toString(), ...pathB.slice(i7)].join("/");
};

// node_modules/zod/v3/external.js
var external_exports = {};
__export(external_exports, {
  BRAND: () => BRAND,
  DIRTY: () => DIRTY,
  EMPTY_PATH: () => EMPTY_PATH,
  INVALID: () => INVALID,
  NEVER: () => NEVER,
  OK: () => OK,
  ParseStatus: () => ParseStatus,
  Schema: () => ZodType,
  ZodAny: () => ZodAny,
  ZodArray: () => ZodArray,
  ZodBigInt: () => ZodBigInt,
  ZodBoolean: () => ZodBoolean,
  ZodBranded: () => ZodBranded,
  ZodCatch: () => ZodCatch,
  ZodDate: () => ZodDate,
  ZodDefault: () => ZodDefault,
  ZodDiscriminatedUnion: () => ZodDiscriminatedUnion,
  ZodEffects: () => ZodEffects,
  ZodEnum: () => ZodEnum,
  ZodError: () => ZodError,
  ZodFirstPartyTypeKind: () => ZodFirstPartyTypeKind,
  ZodFunction: () => ZodFunction,
  ZodIntersection: () => ZodIntersection,
  ZodIssueCode: () => ZodIssueCode,
  ZodLazy: () => ZodLazy,
  ZodLiteral: () => ZodLiteral,
  ZodMap: () => ZodMap,
  ZodNaN: () => ZodNaN,
  ZodNativeEnum: () => ZodNativeEnum,
  ZodNever: () => ZodNever,
  ZodNull: () => ZodNull,
  ZodNullable: () => ZodNullable,
  ZodNumber: () => ZodNumber,
  ZodObject: () => ZodObject,
  ZodOptional: () => ZodOptional,
  ZodParsedType: () => ZodParsedType,
  ZodPipeline: () => ZodPipeline,
  ZodPromise: () => ZodPromise,
  ZodReadonly: () => ZodReadonly,
  ZodRecord: () => ZodRecord,
  ZodSchema: () => ZodType,
  ZodSet: () => ZodSet,
  ZodString: () => ZodString,
  ZodSymbol: () => ZodSymbol,
  ZodTransformer: () => ZodEffects,
  ZodTuple: () => ZodTuple,
  ZodType: () => ZodType,
  ZodUndefined: () => ZodUndefined,
  ZodUnion: () => ZodUnion,
  ZodUnknown: () => ZodUnknown,
  ZodVoid: () => ZodVoid,
  addIssueToContext: () => addIssueToContext,
  any: () => anyType,
  array: () => arrayType,
  bigint: () => bigIntType,
  boolean: () => booleanType,
  coerce: () => coerce,
  custom: () => custom,
  date: () => dateType,
  datetimeRegex: () => datetimeRegex,
  defaultErrorMap: () => en_default,
  discriminatedUnion: () => discriminatedUnionType,
  effect: () => effectsType,
  enum: () => enumType,
  function: () => functionType,
  getErrorMap: () => getErrorMap,
  getParsedType: () => getParsedType,
  instanceof: () => instanceOfType,
  intersection: () => intersectionType,
  isAborted: () => isAborted,
  isAsync: () => isAsync,
  isDirty: () => isDirty,
  isValid: () => isValid,
  late: () => late,
  lazy: () => lazyType,
  literal: () => literalType,
  makeIssue: () => makeIssue,
  map: () => mapType,
  nan: () => nanType,
  nativeEnum: () => nativeEnumType,
  never: () => neverType,
  null: () => nullType,
  nullable: () => nullableType,
  number: () => numberType,
  object: () => objectType,
  objectUtil: () => objectUtil,
  oboolean: () => oboolean,
  onumber: () => onumber,
  optional: () => optionalType,
  ostring: () => ostring,
  pipeline: () => pipelineType,
  preprocess: () => preprocessType,
  promise: () => promiseType,
  quotelessJson: () => quotelessJson,
  record: () => recordType,
  set: () => setType,
  setErrorMap: () => setErrorMap,
  strictObject: () => strictObjectType,
  string: () => stringType,
  symbol: () => symbolType,
  transformer: () => effectsType,
  tuple: () => tupleType,
  undefined: () => undefinedType,
  union: () => unionType,
  unknown: () => unknownType,
  util: () => util,
  void: () => voidType
});

// node_modules/zod/v3/helpers/util.js
var util;
(function(util2) {
  util2.assertEqual = (_3) => {
  };
  function assertIs(_arg) {
  }
  util2.assertIs = assertIs;
  function assertNever(_x) {
    throw new Error();
  }
  util2.assertNever = assertNever;
  util2.arrayToEnum = (items) => {
    const obj = {};
    for (const item of items) {
      obj[item] = item;
    }
    return obj;
  };
  util2.getValidEnumValues = (obj) => {
    const validKeys = util2.objectKeys(obj).filter((k2) => typeof obj[obj[k2]] !== "number");
    const filtered = {};
    for (const k2 of validKeys) {
      filtered[k2] = obj[k2];
    }
    return util2.objectValues(filtered);
  };
  util2.objectValues = (obj) => {
    return util2.objectKeys(obj).map(function(e7) {
      return obj[e7];
    });
  };
  util2.objectKeys = typeof Object.keys === "function" ? (obj) => Object.keys(obj) : (object) => {
    const keys = [];
    for (const key in object) {
      if (Object.prototype.hasOwnProperty.call(object, key)) {
        keys.push(key);
      }
    }
    return keys;
  };
  util2.find = (arr, checker) => {
    for (const item of arr) {
      if (checker(item))
        return item;
    }
    return void 0;
  };
  util2.isInteger = typeof Number.isInteger === "function" ? (val) => Number.isInteger(val) : (val) => typeof val === "number" && Number.isFinite(val) && Math.floor(val) === val;
  function joinValues(array, separator = " | ") {
    return array.map((val) => typeof val === "string" ? `'${val}'` : val).join(separator);
  }
  util2.joinValues = joinValues;
  util2.jsonStringifyReplacer = (_3, value) => {
    if (typeof value === "bigint") {
      return value.toString();
    }
    return value;
  };
})(util || (util = {}));
var objectUtil;
(function(objectUtil2) {
  objectUtil2.mergeShapes = (first, second) => {
    return {
      ...first,
      ...second
      // second overwrites first
    };
  };
})(objectUtil || (objectUtil = {}));
var ZodParsedType = util.arrayToEnum([
  "string",
  "nan",
  "number",
  "integer",
  "float",
  "boolean",
  "date",
  "bigint",
  "symbol",
  "function",
  "undefined",
  "null",
  "array",
  "object",
  "unknown",
  "promise",
  "void",
  "never",
  "map",
  "set"
]);
var getParsedType = (data) => {
  const t7 = typeof data;
  switch (t7) {
    case "undefined":
      return ZodParsedType.undefined;
    case "string":
      return ZodParsedType.string;
    case "number":
      return Number.isNaN(data) ? ZodParsedType.nan : ZodParsedType.number;
    case "boolean":
      return ZodParsedType.boolean;
    case "function":
      return ZodParsedType.function;
    case "bigint":
      return ZodParsedType.bigint;
    case "symbol":
      return ZodParsedType.symbol;
    case "object":
      if (Array.isArray(data)) {
        return ZodParsedType.array;
      }
      if (data === null) {
        return ZodParsedType.null;
      }
      if (data.then && typeof data.then === "function" && data.catch && typeof data.catch === "function") {
        return ZodParsedType.promise;
      }
      if (typeof Map !== "undefined" && data instanceof Map) {
        return ZodParsedType.map;
      }
      if (typeof Set !== "undefined" && data instanceof Set) {
        return ZodParsedType.set;
      }
      if (typeof Date !== "undefined" && data instanceof Date) {
        return ZodParsedType.date;
      }
      return ZodParsedType.object;
    default:
      return ZodParsedType.unknown;
  }
};

// node_modules/zod/v3/ZodError.js
var ZodIssueCode = util.arrayToEnum([
  "invalid_type",
  "invalid_literal",
  "custom",
  "invalid_union",
  "invalid_union_discriminator",
  "invalid_enum_value",
  "unrecognized_keys",
  "invalid_arguments",
  "invalid_return_type",
  "invalid_date",
  "invalid_string",
  "too_small",
  "too_big",
  "invalid_intersection_types",
  "not_multiple_of",
  "not_finite"
]);
var quotelessJson = (obj) => {
  const json = JSON.stringify(obj, null, 2);
  return json.replace(/"([^"]+)":/g, "$1:");
};
var ZodError = class _ZodError extends Error {
  get errors() {
    return this.issues;
  }
  constructor(issues) {
    super();
    this.issues = [];
    this.addIssue = (sub) => {
      this.issues = [...this.issues, sub];
    };
    this.addIssues = (subs = []) => {
      this.issues = [...this.issues, ...subs];
    };
    const actualProto = new.target.prototype;
    if (Object.setPrototypeOf) {
      Object.setPrototypeOf(this, actualProto);
    } else {
      this.__proto__ = actualProto;
    }
    this.name = "ZodError";
    this.issues = issues;
  }
  format(_mapper) {
    const mapper = _mapper || function(issue) {
      return issue.message;
    };
    const fieldErrors = { _errors: [] };
    const processError = (error) => {
      for (const issue of error.issues) {
        if (issue.code === "invalid_union") {
          issue.unionErrors.map(processError);
        } else if (issue.code === "invalid_return_type") {
          processError(issue.returnTypeError);
        } else if (issue.code === "invalid_arguments") {
          processError(issue.argumentsError);
        } else if (issue.path.length === 0) {
          fieldErrors._errors.push(mapper(issue));
        } else {
          let curr = fieldErrors;
          let i7 = 0;
          while (i7 < issue.path.length) {
            const el = issue.path[i7];
            const terminal = i7 === issue.path.length - 1;
            if (!terminal) {
              curr[el] = curr[el] || { _errors: [] };
            } else {
              curr[el] = curr[el] || { _errors: [] };
              curr[el]._errors.push(mapper(issue));
            }
            curr = curr[el];
            i7++;
          }
        }
      }
    };
    processError(this);
    return fieldErrors;
  }
  static assert(value) {
    if (!(value instanceof _ZodError)) {
      throw new Error(`Not a ZodError: ${value}`);
    }
  }
  toString() {
    return this.message;
  }
  get message() {
    return JSON.stringify(this.issues, util.jsonStringifyReplacer, 2);
  }
  get isEmpty() {
    return this.issues.length === 0;
  }
  flatten(mapper = (issue) => issue.message) {
    const fieldErrors = {};
    const formErrors = [];
    for (const sub of this.issues) {
      if (sub.path.length > 0) {
        const firstEl = sub.path[0];
        fieldErrors[firstEl] = fieldErrors[firstEl] || [];
        fieldErrors[firstEl].push(mapper(sub));
      } else {
        formErrors.push(mapper(sub));
      }
    }
    return { formErrors, fieldErrors };
  }
  get formErrors() {
    return this.flatten();
  }
};
ZodError.create = (issues) => {
  const error = new ZodError(issues);
  return error;
};

// node_modules/zod/v3/locales/en.js
var errorMap = (issue, _ctx) => {
  let message;
  switch (issue.code) {
    case ZodIssueCode.invalid_type:
      if (issue.received === ZodParsedType.undefined) {
        message = "Required";
      } else {
        message = `Expected ${issue.expected}, received ${issue.received}`;
      }
      break;
    case ZodIssueCode.invalid_literal:
      message = `Invalid literal value, expected ${JSON.stringify(issue.expected, util.jsonStringifyReplacer)}`;
      break;
    case ZodIssueCode.unrecognized_keys:
      message = `Unrecognized key(s) in object: ${util.joinValues(issue.keys, ", ")}`;
      break;
    case ZodIssueCode.invalid_union:
      message = `Invalid input`;
      break;
    case ZodIssueCode.invalid_union_discriminator:
      message = `Invalid discriminator value. Expected ${util.joinValues(issue.options)}`;
      break;
    case ZodIssueCode.invalid_enum_value:
      message = `Invalid enum value. Expected ${util.joinValues(issue.options)}, received '${issue.received}'`;
      break;
    case ZodIssueCode.invalid_arguments:
      message = `Invalid function arguments`;
      break;
    case ZodIssueCode.invalid_return_type:
      message = `Invalid function return type`;
      break;
    case ZodIssueCode.invalid_date:
      message = `Invalid date`;
      break;
    case ZodIssueCode.invalid_string:
      if (typeof issue.validation === "object") {
        if ("includes" in issue.validation) {
          message = `Invalid input: must include "${issue.validation.includes}"`;
          if (typeof issue.validation.position === "number") {
            message = `${message} at one or more positions greater than or equal to ${issue.validation.position}`;
          }
        } else if ("startsWith" in issue.validation) {
          message = `Invalid input: must start with "${issue.validation.startsWith}"`;
        } else if ("endsWith" in issue.validation) {
          message = `Invalid input: must end with "${issue.validation.endsWith}"`;
        } else {
          util.assertNever(issue.validation);
        }
      } else if (issue.validation !== "regex") {
        message = `Invalid ${issue.validation}`;
      } else {
        message = "Invalid";
      }
      break;
    case ZodIssueCode.too_small:
      if (issue.type === "array")
        message = `Array must contain ${issue.exact ? "exactly" : issue.inclusive ? `at least` : `more than`} ${issue.minimum} element(s)`;
      else if (issue.type === "string")
        message = `String must contain ${issue.exact ? "exactly" : issue.inclusive ? `at least` : `over`} ${issue.minimum} character(s)`;
      else if (issue.type === "number")
        message = `Number must be ${issue.exact ? `exactly equal to ` : issue.inclusive ? `greater than or equal to ` : `greater than `}${issue.minimum}`;
      else if (issue.type === "bigint")
        message = `Number must be ${issue.exact ? `exactly equal to ` : issue.inclusive ? `greater than or equal to ` : `greater than `}${issue.minimum}`;
      else if (issue.type === "date")
        message = `Date must be ${issue.exact ? `exactly equal to ` : issue.inclusive ? `greater than or equal to ` : `greater than `}${new Date(Number(issue.minimum))}`;
      else
        message = "Invalid input";
      break;
    case ZodIssueCode.too_big:
      if (issue.type === "array")
        message = `Array must contain ${issue.exact ? `exactly` : issue.inclusive ? `at most` : `less than`} ${issue.maximum} element(s)`;
      else if (issue.type === "string")
        message = `String must contain ${issue.exact ? `exactly` : issue.inclusive ? `at most` : `under`} ${issue.maximum} character(s)`;
      else if (issue.type === "number")
        message = `Number must be ${issue.exact ? `exactly` : issue.inclusive ? `less than or equal to` : `less than`} ${issue.maximum}`;
      else if (issue.type === "bigint")
        message = `BigInt must be ${issue.exact ? `exactly` : issue.inclusive ? `less than or equal to` : `less than`} ${issue.maximum}`;
      else if (issue.type === "date")
        message = `Date must be ${issue.exact ? `exactly` : issue.inclusive ? `smaller than or equal to` : `smaller than`} ${new Date(Number(issue.maximum))}`;
      else
        message = "Invalid input";
      break;
    case ZodIssueCode.custom:
      message = `Invalid input`;
      break;
    case ZodIssueCode.invalid_intersection_types:
      message = `Intersection results could not be merged`;
      break;
    case ZodIssueCode.not_multiple_of:
      message = `Number must be a multiple of ${issue.multipleOf}`;
      break;
    case ZodIssueCode.not_finite:
      message = "Number must be finite";
      break;
    default:
      message = _ctx.defaultError;
      util.assertNever(issue);
  }
  return { message };
};
var en_default = errorMap;

// node_modules/zod/v3/errors.js
var overrideErrorMap = en_default;
function setErrorMap(map2) {
  overrideErrorMap = map2;
}
function getErrorMap() {
  return overrideErrorMap;
}

// node_modules/zod/v3/helpers/parseUtil.js
var makeIssue = (params) => {
  const { data, path, errorMaps, issueData } = params;
  const fullPath = [...path, ...issueData.path || []];
  const fullIssue = {
    ...issueData,
    path: fullPath
  };
  if (issueData.message !== void 0) {
    return {
      ...issueData,
      path: fullPath,
      message: issueData.message
    };
  }
  let errorMessage = "";
  const maps = errorMaps.filter((m4) => !!m4).slice().reverse();
  for (const map2 of maps) {
    errorMessage = map2(fullIssue, { data, defaultError: errorMessage }).message;
  }
  return {
    ...issueData,
    path: fullPath,
    message: errorMessage
  };
};
var EMPTY_PATH = [];
function addIssueToContext(ctx, issueData) {
  const overrideMap = getErrorMap();
  const issue = makeIssue({
    issueData,
    data: ctx.data,
    path: ctx.path,
    errorMaps: [
      ctx.common.contextualErrorMap,
      // contextual error map is first priority
      ctx.schemaErrorMap,
      // then schema-bound map if available
      overrideMap,
      // then global override map
      overrideMap === en_default ? void 0 : en_default
      // then global default map
    ].filter((x3) => !!x3)
  });
  ctx.common.issues.push(issue);
}
var ParseStatus = class _ParseStatus {
  constructor() {
    this.value = "valid";
  }
  dirty() {
    if (this.value === "valid")
      this.value = "dirty";
  }
  abort() {
    if (this.value !== "aborted")
      this.value = "aborted";
  }
  static mergeArray(status, results) {
    const arrayValue = [];
    for (const s5 of results) {
      if (s5.status === "aborted")
        return INVALID;
      if (s5.status === "dirty")
        status.dirty();
      arrayValue.push(s5.value);
    }
    return { status: status.value, value: arrayValue };
  }
  static async mergeObjectAsync(status, pairs) {
    const syncPairs = [];
    for (const pair of pairs) {
      const key = await pair.key;
      const value = await pair.value;
      syncPairs.push({
        key,
        value
      });
    }
    return _ParseStatus.mergeObjectSync(status, syncPairs);
  }
  static mergeObjectSync(status, pairs) {
    const finalObject = {};
    for (const pair of pairs) {
      const { key, value } = pair;
      if (key.status === "aborted")
        return INVALID;
      if (value.status === "aborted")
        return INVALID;
      if (key.status === "dirty")
        status.dirty();
      if (value.status === "dirty")
        status.dirty();
      if (key.value !== "__proto__" && (typeof value.value !== "undefined" || pair.alwaysSet)) {
        finalObject[key.value] = value.value;
      }
    }
    return { status: status.value, value: finalObject };
  }
};
var INVALID = Object.freeze({
  status: "aborted"
});
var DIRTY = (value) => ({ status: "dirty", value });
var OK = (value) => ({ status: "valid", value });
var isAborted = (x3) => x3.status === "aborted";
var isDirty = (x3) => x3.status === "dirty";
var isValid = (x3) => x3.status === "valid";
var isAsync = (x3) => typeof Promise !== "undefined" && x3 instanceof Promise;

// node_modules/zod/v3/helpers/errorUtil.js
var errorUtil;
(function(errorUtil2) {
  errorUtil2.errToObj = (message) => typeof message === "string" ? { message } : message || {};
  errorUtil2.toString = (message) => typeof message === "string" ? message : message?.message;
})(errorUtil || (errorUtil = {}));

// node_modules/zod/v3/types.js
var ParseInputLazyPath = class {
  constructor(parent, value, path, key) {
    this._cachedPath = [];
    this.parent = parent;
    this.data = value;
    this._path = path;
    this._key = key;
  }
  get path() {
    if (!this._cachedPath.length) {
      if (Array.isArray(this._key)) {
        this._cachedPath.push(...this._path, ...this._key);
      } else {
        this._cachedPath.push(...this._path, this._key);
      }
    }
    return this._cachedPath;
  }
};
var handleResult = (ctx, result) => {
  if (isValid(result)) {
    return { success: true, data: result.value };
  } else {
    if (!ctx.common.issues.length) {
      throw new Error("Validation failed but no issues detected.");
    }
    return {
      success: false,
      get error() {
        if (this._error)
          return this._error;
        const error = new ZodError(ctx.common.issues);
        this._error = error;
        return this._error;
      }
    };
  }
};
function processCreateParams(params) {
  if (!params)
    return {};
  const { errorMap: errorMap2, invalid_type_error, required_error, description } = params;
  if (errorMap2 && (invalid_type_error || required_error)) {
    throw new Error(`Can't use "invalid_type_error" or "required_error" in conjunction with custom error map.`);
  }
  if (errorMap2)
    return { errorMap: errorMap2, description };
  const customMap = (iss, ctx) => {
    const { message } = params;
    if (iss.code === "invalid_enum_value") {
      return { message: message ?? ctx.defaultError };
    }
    if (typeof ctx.data === "undefined") {
      return { message: message ?? required_error ?? ctx.defaultError };
    }
    if (iss.code !== "invalid_type")
      return { message: ctx.defaultError };
    return { message: message ?? invalid_type_error ?? ctx.defaultError };
  };
  return { errorMap: customMap, description };
}
var ZodType = class {
  get description() {
    return this._def.description;
  }
  _getType(input) {
    return getParsedType(input.data);
  }
  _getOrReturnCtx(input, ctx) {
    return ctx || {
      common: input.parent.common,
      data: input.data,
      parsedType: getParsedType(input.data),
      schemaErrorMap: this._def.errorMap,
      path: input.path,
      parent: input.parent
    };
  }
  _processInputParams(input) {
    return {
      status: new ParseStatus(),
      ctx: {
        common: input.parent.common,
        data: input.data,
        parsedType: getParsedType(input.data),
        schemaErrorMap: this._def.errorMap,
        path: input.path,
        parent: input.parent
      }
    };
  }
  _parseSync(input) {
    const result = this._parse(input);
    if (isAsync(result)) {
      throw new Error("Synchronous parse encountered promise.");
    }
    return result;
  }
  _parseAsync(input) {
    const result = this._parse(input);
    return Promise.resolve(result);
  }
  parse(data, params) {
    const result = this.safeParse(data, params);
    if (result.success)
      return result.data;
    throw result.error;
  }
  safeParse(data, params) {
    const ctx = {
      common: {
        issues: [],
        async: params?.async ?? false,
        contextualErrorMap: params?.errorMap
      },
      path: params?.path || [],
      schemaErrorMap: this._def.errorMap,
      parent: null,
      data,
      parsedType: getParsedType(data)
    };
    const result = this._parseSync({ data, path: ctx.path, parent: ctx });
    return handleResult(ctx, result);
  }
  "~validate"(data) {
    const ctx = {
      common: {
        issues: [],
        async: !!this["~standard"].async
      },
      path: [],
      schemaErrorMap: this._def.errorMap,
      parent: null,
      data,
      parsedType: getParsedType(data)
    };
    if (!this["~standard"].async) {
      try {
        const result = this._parseSync({ data, path: [], parent: ctx });
        return isValid(result) ? {
          value: result.value
        } : {
          issues: ctx.common.issues
        };
      } catch (err) {
        if (err?.message?.toLowerCase()?.includes("encountered")) {
          this["~standard"].async = true;
        }
        ctx.common = {
          issues: [],
          async: true
        };
      }
    }
    return this._parseAsync({ data, path: [], parent: ctx }).then((result) => isValid(result) ? {
      value: result.value
    } : {
      issues: ctx.common.issues
    });
  }
  async parseAsync(data, params) {
    const result = await this.safeParseAsync(data, params);
    if (result.success)
      return result.data;
    throw result.error;
  }
  async safeParseAsync(data, params) {
    const ctx = {
      common: {
        issues: [],
        contextualErrorMap: params?.errorMap,
        async: true
      },
      path: params?.path || [],
      schemaErrorMap: this._def.errorMap,
      parent: null,
      data,
      parsedType: getParsedType(data)
    };
    const maybeAsyncResult = this._parse({ data, path: ctx.path, parent: ctx });
    const result = await (isAsync(maybeAsyncResult) ? maybeAsyncResult : Promise.resolve(maybeAsyncResult));
    return handleResult(ctx, result);
  }
  refine(check, message) {
    const getIssueProperties = (val) => {
      if (typeof message === "string" || typeof message === "undefined") {
        return { message };
      } else if (typeof message === "function") {
        return message(val);
      } else {
        return message;
      }
    };
    return this._refinement((val, ctx) => {
      const result = check(val);
      const setError = () => ctx.addIssue({
        code: ZodIssueCode.custom,
        ...getIssueProperties(val)
      });
      if (typeof Promise !== "undefined" && result instanceof Promise) {
        return result.then((data) => {
          if (!data) {
            setError();
            return false;
          } else {
            return true;
          }
        });
      }
      if (!result) {
        setError();
        return false;
      } else {
        return true;
      }
    });
  }
  refinement(check, refinementData) {
    return this._refinement((val, ctx) => {
      if (!check(val)) {
        ctx.addIssue(typeof refinementData === "function" ? refinementData(val, ctx) : refinementData);
        return false;
      } else {
        return true;
      }
    });
  }
  _refinement(refinement) {
    return new ZodEffects({
      schema: this,
      typeName: ZodFirstPartyTypeKind.ZodEffects,
      effect: { type: "refinement", refinement }
    });
  }
  superRefine(refinement) {
    return this._refinement(refinement);
  }
  constructor(def) {
    this.spa = this.safeParseAsync;
    this._def = def;
    this.parse = this.parse.bind(this);
    this.safeParse = this.safeParse.bind(this);
    this.parseAsync = this.parseAsync.bind(this);
    this.safeParseAsync = this.safeParseAsync.bind(this);
    this.spa = this.spa.bind(this);
    this.refine = this.refine.bind(this);
    this.refinement = this.refinement.bind(this);
    this.superRefine = this.superRefine.bind(this);
    this.optional = this.optional.bind(this);
    this.nullable = this.nullable.bind(this);
    this.nullish = this.nullish.bind(this);
    this.array = this.array.bind(this);
    this.promise = this.promise.bind(this);
    this.or = this.or.bind(this);
    this.and = this.and.bind(this);
    this.transform = this.transform.bind(this);
    this.brand = this.brand.bind(this);
    this.default = this.default.bind(this);
    this.catch = this.catch.bind(this);
    this.describe = this.describe.bind(this);
    this.pipe = this.pipe.bind(this);
    this.readonly = this.readonly.bind(this);
    this.isNullable = this.isNullable.bind(this);
    this.isOptional = this.isOptional.bind(this);
    this["~standard"] = {
      version: 1,
      vendor: "zod",
      validate: (data) => this["~validate"](data)
    };
  }
  optional() {
    return ZodOptional.create(this, this._def);
  }
  nullable() {
    return ZodNullable.create(this, this._def);
  }
  nullish() {
    return this.nullable().optional();
  }
  array() {
    return ZodArray.create(this);
  }
  promise() {
    return ZodPromise.create(this, this._def);
  }
  or(option) {
    return ZodUnion.create([this, option], this._def);
  }
  and(incoming) {
    return ZodIntersection.create(this, incoming, this._def);
  }
  transform(transform) {
    return new ZodEffects({
      ...processCreateParams(this._def),
      schema: this,
      typeName: ZodFirstPartyTypeKind.ZodEffects,
      effect: { type: "transform", transform }
    });
  }
  default(def) {
    const defaultValueFunc = typeof def === "function" ? def : () => def;
    return new ZodDefault({
      ...processCreateParams(this._def),
      innerType: this,
      defaultValue: defaultValueFunc,
      typeName: ZodFirstPartyTypeKind.ZodDefault
    });
  }
  brand() {
    return new ZodBranded({
      typeName: ZodFirstPartyTypeKind.ZodBranded,
      type: this,
      ...processCreateParams(this._def)
    });
  }
  catch(def) {
    const catchValueFunc = typeof def === "function" ? def : () => def;
    return new ZodCatch({
      ...processCreateParams(this._def),
      innerType: this,
      catchValue: catchValueFunc,
      typeName: ZodFirstPartyTypeKind.ZodCatch
    });
  }
  describe(description) {
    const This = this.constructor;
    return new This({
      ...this._def,
      description
    });
  }
  pipe(target) {
    return ZodPipeline.create(this, target);
  }
  readonly() {
    return ZodReadonly.create(this);
  }
  isOptional() {
    return this.safeParse(void 0).success;
  }
  isNullable() {
    return this.safeParse(null).success;
  }
};
var cuidRegex = /^c[^\s-]{8,}$/i;
var cuid2Regex = /^[0-9a-z]+$/;
var ulidRegex = /^[0-9A-HJKMNP-TV-Z]{26}$/i;
var uuidRegex = /^[0-9a-fA-F]{8}\b-[0-9a-fA-F]{4}\b-[0-9a-fA-F]{4}\b-[0-9a-fA-F]{4}\b-[0-9a-fA-F]{12}$/i;
var nanoidRegex = /^[a-z0-9_-]{21}$/i;
var jwtRegex = /^[A-Za-z0-9-_]+\.[A-Za-z0-9-_]+\.[A-Za-z0-9-_]*$/;
var durationRegex = /^[-+]?P(?!$)(?:(?:[-+]?\d+Y)|(?:[-+]?\d+[.,]\d+Y$))?(?:(?:[-+]?\d+M)|(?:[-+]?\d+[.,]\d+M$))?(?:(?:[-+]?\d+W)|(?:[-+]?\d+[.,]\d+W$))?(?:(?:[-+]?\d+D)|(?:[-+]?\d+[.,]\d+D$))?(?:T(?=[\d+-])(?:(?:[-+]?\d+H)|(?:[-+]?\d+[.,]\d+H$))?(?:(?:[-+]?\d+M)|(?:[-+]?\d+[.,]\d+M$))?(?:[-+]?\d+(?:[.,]\d+)?S)?)??$/;
var emailRegex = /^(?!\.)(?!.*\.\.)([A-Z0-9_'+\-\.]*)[A-Z0-9_+-]@([A-Z0-9][A-Z0-9\-]*\.)+[A-Z]{2,}$/i;
var _emojiRegex = `^(\\p{Extended_Pictographic}|\\p{Emoji_Component})+$`;
var emojiRegex;
var ipv4Regex = /^(?:(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])$/;
var ipv4CidrRegex = /^(?:(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])\/(3[0-2]|[12]?[0-9])$/;
var ipv6Regex = /^(([0-9a-fA-F]{1,4}:){7,7}[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,7}:|([0-9a-fA-F]{1,4}:){1,6}:[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,5}(:[0-9a-fA-F]{1,4}){1,2}|([0-9a-fA-F]{1,4}:){1,4}(:[0-9a-fA-F]{1,4}){1,3}|([0-9a-fA-F]{1,4}:){1,3}(:[0-9a-fA-F]{1,4}){1,4}|([0-9a-fA-F]{1,4}:){1,2}(:[0-9a-fA-F]{1,4}){1,5}|[0-9a-fA-F]{1,4}:((:[0-9a-fA-F]{1,4}){1,6})|:((:[0-9a-fA-F]{1,4}){1,7}|:)|fe80:(:[0-9a-fA-F]{0,4}){0,4}%[0-9a-zA-Z]{1,}|::(ffff(:0{1,4}){0,1}:){0,1}((25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])|([0-9a-fA-F]{1,4}:){1,4}:((25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9]))$/;
var ipv6CidrRegex = /^(([0-9a-fA-F]{1,4}:){7,7}[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,7}:|([0-9a-fA-F]{1,4}:){1,6}:[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,5}(:[0-9a-fA-F]{1,4}){1,2}|([0-9a-fA-F]{1,4}:){1,4}(:[0-9a-fA-F]{1,4}){1,3}|([0-9a-fA-F]{1,4}:){1,3}(:[0-9a-fA-F]{1,4}){1,4}|([0-9a-fA-F]{1,4}:){1,2}(:[0-9a-fA-F]{1,4}){1,5}|[0-9a-fA-F]{1,4}:((:[0-9a-fA-F]{1,4}){1,6})|:((:[0-9a-fA-F]{1,4}){1,7}|:)|fe80:(:[0-9a-fA-F]{0,4}){0,4}%[0-9a-zA-Z]{1,}|::(ffff(:0{1,4}){0,1}:){0,1}((25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])|([0-9a-fA-F]{1,4}:){1,4}:((25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9]))\/(12[0-8]|1[01][0-9]|[1-9]?[0-9])$/;
var base64Regex = /^([0-9a-zA-Z+/]{4})*(([0-9a-zA-Z+/]{2}==)|([0-9a-zA-Z+/]{3}=))?$/;
var base64urlRegex = /^([0-9a-zA-Z-_]{4})*(([0-9a-zA-Z-_]{2}(==)?)|([0-9a-zA-Z-_]{3}(=)?))?$/;
var dateRegexSource = `((\\d\\d[2468][048]|\\d\\d[13579][26]|\\d\\d0[48]|[02468][048]00|[13579][26]00)-02-29|\\d{4}-((0[13578]|1[02])-(0[1-9]|[12]\\d|3[01])|(0[469]|11)-(0[1-9]|[12]\\d|30)|(02)-(0[1-9]|1\\d|2[0-8])))`;
var dateRegex = new RegExp(`^${dateRegexSource}$`);
function timeRegexSource(args) {
  let secondsRegexSource = `[0-5]\\d`;
  if (args.precision) {
    secondsRegexSource = `${secondsRegexSource}\\.\\d{${args.precision}}`;
  } else if (args.precision == null) {
    secondsRegexSource = `${secondsRegexSource}(\\.\\d+)?`;
  }
  const secondsQuantifier = args.precision ? "+" : "?";
  return `([01]\\d|2[0-3]):[0-5]\\d(:${secondsRegexSource})${secondsQuantifier}`;
}
function timeRegex(args) {
  return new RegExp(`^${timeRegexSource(args)}$`);
}
function datetimeRegex(args) {
  let regex = `${dateRegexSource}T${timeRegexSource(args)}`;
  const opts = [];
  opts.push(args.local ? `Z?` : `Z`);
  if (args.offset)
    opts.push(`([+-]\\d{2}:?\\d{2})`);
  regex = `${regex}(${opts.join("|")})`;
  return new RegExp(`^${regex}$`);
}
function isValidIP(ip, version) {
  if ((version === "v4" || !version) && ipv4Regex.test(ip)) {
    return true;
  }
  if ((version === "v6" || !version) && ipv6Regex.test(ip)) {
    return true;
  }
  return false;
}
function isValidJWT(jwt, alg) {
  if (!jwtRegex.test(jwt))
    return false;
  try {
    const [header] = jwt.split(".");
    if (!header)
      return false;
    const base64 = header.replace(/-/g, "+").replace(/_/g, "/").padEnd(header.length + (4 - header.length % 4) % 4, "=");
    const decoded = JSON.parse(atob(base64));
    if (typeof decoded !== "object" || decoded === null)
      return false;
    if ("typ" in decoded && decoded?.typ !== "JWT")
      return false;
    if (!decoded.alg)
      return false;
    if (alg && decoded.alg !== alg)
      return false;
    return true;
  } catch {
    return false;
  }
}
function isValidCidr(ip, version) {
  if ((version === "v4" || !version) && ipv4CidrRegex.test(ip)) {
    return true;
  }
  if ((version === "v6" || !version) && ipv6CidrRegex.test(ip)) {
    return true;
  }
  return false;
}
var ZodString = class _ZodString extends ZodType {
  _parse(input) {
    if (this._def.coerce) {
      input.data = String(input.data);
    }
    const parsedType = this._getType(input);
    if (parsedType !== ZodParsedType.string) {
      const ctx2 = this._getOrReturnCtx(input);
      addIssueToContext(ctx2, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.string,
        received: ctx2.parsedType
      });
      return INVALID;
    }
    const status = new ParseStatus();
    let ctx = void 0;
    for (const check of this._def.checks) {
      if (check.kind === "min") {
        if (input.data.length < check.value) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.too_small,
            minimum: check.value,
            type: "string",
            inclusive: true,
            exact: false,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "max") {
        if (input.data.length > check.value) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.too_big,
            maximum: check.value,
            type: "string",
            inclusive: true,
            exact: false,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "length") {
        const tooBig = input.data.length > check.value;
        const tooSmall = input.data.length < check.value;
        if (tooBig || tooSmall) {
          ctx = this._getOrReturnCtx(input, ctx);
          if (tooBig) {
            addIssueToContext(ctx, {
              code: ZodIssueCode.too_big,
              maximum: check.value,
              type: "string",
              inclusive: true,
              exact: true,
              message: check.message
            });
          } else if (tooSmall) {
            addIssueToContext(ctx, {
              code: ZodIssueCode.too_small,
              minimum: check.value,
              type: "string",
              inclusive: true,
              exact: true,
              message: check.message
            });
          }
          status.dirty();
        }
      } else if (check.kind === "email") {
        if (!emailRegex.test(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "email",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "emoji") {
        if (!emojiRegex) {
          emojiRegex = new RegExp(_emojiRegex, "u");
        }
        if (!emojiRegex.test(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "emoji",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "uuid") {
        if (!uuidRegex.test(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "uuid",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "nanoid") {
        if (!nanoidRegex.test(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "nanoid",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "cuid") {
        if (!cuidRegex.test(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "cuid",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "cuid2") {
        if (!cuid2Regex.test(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "cuid2",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "ulid") {
        if (!ulidRegex.test(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "ulid",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "url") {
        try {
          new URL(input.data);
        } catch {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "url",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "regex") {
        check.regex.lastIndex = 0;
        const testResult = check.regex.test(input.data);
        if (!testResult) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "regex",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "trim") {
        input.data = input.data.trim();
      } else if (check.kind === "includes") {
        if (!input.data.includes(check.value, check.position)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.invalid_string,
            validation: { includes: check.value, position: check.position },
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "toLowerCase") {
        input.data = input.data.toLowerCase();
      } else if (check.kind === "toUpperCase") {
        input.data = input.data.toUpperCase();
      } else if (check.kind === "startsWith") {
        if (!input.data.startsWith(check.value)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.invalid_string,
            validation: { startsWith: check.value },
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "endsWith") {
        if (!input.data.endsWith(check.value)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.invalid_string,
            validation: { endsWith: check.value },
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "datetime") {
        const regex = datetimeRegex(check);
        if (!regex.test(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.invalid_string,
            validation: "datetime",
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "date") {
        const regex = dateRegex;
        if (!regex.test(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.invalid_string,
            validation: "date",
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "time") {
        const regex = timeRegex(check);
        if (!regex.test(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.invalid_string,
            validation: "time",
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "duration") {
        if (!durationRegex.test(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "duration",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "ip") {
        if (!isValidIP(input.data, check.version)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "ip",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "jwt") {
        if (!isValidJWT(input.data, check.alg)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "jwt",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "cidr") {
        if (!isValidCidr(input.data, check.version)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "cidr",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "base64") {
        if (!base64Regex.test(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "base64",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "base64url") {
        if (!base64urlRegex.test(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            validation: "base64url",
            code: ZodIssueCode.invalid_string,
            message: check.message
          });
          status.dirty();
        }
      } else {
        util.assertNever(check);
      }
    }
    return { status: status.value, value: input.data };
  }
  _regex(regex, validation, message) {
    return this.refinement((data) => regex.test(data), {
      validation,
      code: ZodIssueCode.invalid_string,
      ...errorUtil.errToObj(message)
    });
  }
  _addCheck(check) {
    return new _ZodString({
      ...this._def,
      checks: [...this._def.checks, check]
    });
  }
  email(message) {
    return this._addCheck({ kind: "email", ...errorUtil.errToObj(message) });
  }
  url(message) {
    return this._addCheck({ kind: "url", ...errorUtil.errToObj(message) });
  }
  emoji(message) {
    return this._addCheck({ kind: "emoji", ...errorUtil.errToObj(message) });
  }
  uuid(message) {
    return this._addCheck({ kind: "uuid", ...errorUtil.errToObj(message) });
  }
  nanoid(message) {
    return this._addCheck({ kind: "nanoid", ...errorUtil.errToObj(message) });
  }
  cuid(message) {
    return this._addCheck({ kind: "cuid", ...errorUtil.errToObj(message) });
  }
  cuid2(message) {
    return this._addCheck({ kind: "cuid2", ...errorUtil.errToObj(message) });
  }
  ulid(message) {
    return this._addCheck({ kind: "ulid", ...errorUtil.errToObj(message) });
  }
  base64(message) {
    return this._addCheck({ kind: "base64", ...errorUtil.errToObj(message) });
  }
  base64url(message) {
    return this._addCheck({
      kind: "base64url",
      ...errorUtil.errToObj(message)
    });
  }
  jwt(options) {
    return this._addCheck({ kind: "jwt", ...errorUtil.errToObj(options) });
  }
  ip(options) {
    return this._addCheck({ kind: "ip", ...errorUtil.errToObj(options) });
  }
  cidr(options) {
    return this._addCheck({ kind: "cidr", ...errorUtil.errToObj(options) });
  }
  datetime(options) {
    if (typeof options === "string") {
      return this._addCheck({
        kind: "datetime",
        precision: null,
        offset: false,
        local: false,
        message: options
      });
    }
    return this._addCheck({
      kind: "datetime",
      precision: typeof options?.precision === "undefined" ? null : options?.precision,
      offset: options?.offset ?? false,
      local: options?.local ?? false,
      ...errorUtil.errToObj(options?.message)
    });
  }
  date(message) {
    return this._addCheck({ kind: "date", message });
  }
  time(options) {
    if (typeof options === "string") {
      return this._addCheck({
        kind: "time",
        precision: null,
        message: options
      });
    }
    return this._addCheck({
      kind: "time",
      precision: typeof options?.precision === "undefined" ? null : options?.precision,
      ...errorUtil.errToObj(options?.message)
    });
  }
  duration(message) {
    return this._addCheck({ kind: "duration", ...errorUtil.errToObj(message) });
  }
  regex(regex, message) {
    return this._addCheck({
      kind: "regex",
      regex,
      ...errorUtil.errToObj(message)
    });
  }
  includes(value, options) {
    return this._addCheck({
      kind: "includes",
      value,
      position: options?.position,
      ...errorUtil.errToObj(options?.message)
    });
  }
  startsWith(value, message) {
    return this._addCheck({
      kind: "startsWith",
      value,
      ...errorUtil.errToObj(message)
    });
  }
  endsWith(value, message) {
    return this._addCheck({
      kind: "endsWith",
      value,
      ...errorUtil.errToObj(message)
    });
  }
  min(minLength, message) {
    return this._addCheck({
      kind: "min",
      value: minLength,
      ...errorUtil.errToObj(message)
    });
  }
  max(maxLength, message) {
    return this._addCheck({
      kind: "max",
      value: maxLength,
      ...errorUtil.errToObj(message)
    });
  }
  length(len, message) {
    return this._addCheck({
      kind: "length",
      value: len,
      ...errorUtil.errToObj(message)
    });
  }
  /**
   * Equivalent to `.min(1)`
   */
  nonempty(message) {
    return this.min(1, errorUtil.errToObj(message));
  }
  trim() {
    return new _ZodString({
      ...this._def,
      checks: [...this._def.checks, { kind: "trim" }]
    });
  }
  toLowerCase() {
    return new _ZodString({
      ...this._def,
      checks: [...this._def.checks, { kind: "toLowerCase" }]
    });
  }
  toUpperCase() {
    return new _ZodString({
      ...this._def,
      checks: [...this._def.checks, { kind: "toUpperCase" }]
    });
  }
  get isDatetime() {
    return !!this._def.checks.find((ch) => ch.kind === "datetime");
  }
  get isDate() {
    return !!this._def.checks.find((ch) => ch.kind === "date");
  }
  get isTime() {
    return !!this._def.checks.find((ch) => ch.kind === "time");
  }
  get isDuration() {
    return !!this._def.checks.find((ch) => ch.kind === "duration");
  }
  get isEmail() {
    return !!this._def.checks.find((ch) => ch.kind === "email");
  }
  get isURL() {
    return !!this._def.checks.find((ch) => ch.kind === "url");
  }
  get isEmoji() {
    return !!this._def.checks.find((ch) => ch.kind === "emoji");
  }
  get isUUID() {
    return !!this._def.checks.find((ch) => ch.kind === "uuid");
  }
  get isNANOID() {
    return !!this._def.checks.find((ch) => ch.kind === "nanoid");
  }
  get isCUID() {
    return !!this._def.checks.find((ch) => ch.kind === "cuid");
  }
  get isCUID2() {
    return !!this._def.checks.find((ch) => ch.kind === "cuid2");
  }
  get isULID() {
    return !!this._def.checks.find((ch) => ch.kind === "ulid");
  }
  get isIP() {
    return !!this._def.checks.find((ch) => ch.kind === "ip");
  }
  get isCIDR() {
    return !!this._def.checks.find((ch) => ch.kind === "cidr");
  }
  get isBase64() {
    return !!this._def.checks.find((ch) => ch.kind === "base64");
  }
  get isBase64url() {
    return !!this._def.checks.find((ch) => ch.kind === "base64url");
  }
  get minLength() {
    let min = null;
    for (const ch of this._def.checks) {
      if (ch.kind === "min") {
        if (min === null || ch.value > min)
          min = ch.value;
      }
    }
    return min;
  }
  get maxLength() {
    let max = null;
    for (const ch of this._def.checks) {
      if (ch.kind === "max") {
        if (max === null || ch.value < max)
          max = ch.value;
      }
    }
    return max;
  }
};
ZodString.create = (params) => {
  return new ZodString({
    checks: [],
    typeName: ZodFirstPartyTypeKind.ZodString,
    coerce: params?.coerce ?? false,
    ...processCreateParams(params)
  });
};
function floatSafeRemainder(val, step) {
  const valDecCount = (val.toString().split(".")[1] || "").length;
  const stepDecCount = (step.toString().split(".")[1] || "").length;
  const decCount = valDecCount > stepDecCount ? valDecCount : stepDecCount;
  const valInt = Number.parseInt(val.toFixed(decCount).replace(".", ""));
  const stepInt = Number.parseInt(step.toFixed(decCount).replace(".", ""));
  return valInt % stepInt / 10 ** decCount;
}
var ZodNumber = class _ZodNumber extends ZodType {
  constructor() {
    super(...arguments);
    this.min = this.gte;
    this.max = this.lte;
    this.step = this.multipleOf;
  }
  _parse(input) {
    if (this._def.coerce) {
      input.data = Number(input.data);
    }
    const parsedType = this._getType(input);
    if (parsedType !== ZodParsedType.number) {
      const ctx2 = this._getOrReturnCtx(input);
      addIssueToContext(ctx2, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.number,
        received: ctx2.parsedType
      });
      return INVALID;
    }
    let ctx = void 0;
    const status = new ParseStatus();
    for (const check of this._def.checks) {
      if (check.kind === "int") {
        if (!util.isInteger(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.invalid_type,
            expected: "integer",
            received: "float",
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "min") {
        const tooSmall = check.inclusive ? input.data < check.value : input.data <= check.value;
        if (tooSmall) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.too_small,
            minimum: check.value,
            type: "number",
            inclusive: check.inclusive,
            exact: false,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "max") {
        const tooBig = check.inclusive ? input.data > check.value : input.data >= check.value;
        if (tooBig) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.too_big,
            maximum: check.value,
            type: "number",
            inclusive: check.inclusive,
            exact: false,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "multipleOf") {
        if (floatSafeRemainder(input.data, check.value) !== 0) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.not_multiple_of,
            multipleOf: check.value,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "finite") {
        if (!Number.isFinite(input.data)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.not_finite,
            message: check.message
          });
          status.dirty();
        }
      } else {
        util.assertNever(check);
      }
    }
    return { status: status.value, value: input.data };
  }
  gte(value, message) {
    return this.setLimit("min", value, true, errorUtil.toString(message));
  }
  gt(value, message) {
    return this.setLimit("min", value, false, errorUtil.toString(message));
  }
  lte(value, message) {
    return this.setLimit("max", value, true, errorUtil.toString(message));
  }
  lt(value, message) {
    return this.setLimit("max", value, false, errorUtil.toString(message));
  }
  setLimit(kind, value, inclusive, message) {
    return new _ZodNumber({
      ...this._def,
      checks: [
        ...this._def.checks,
        {
          kind,
          value,
          inclusive,
          message: errorUtil.toString(message)
        }
      ]
    });
  }
  _addCheck(check) {
    return new _ZodNumber({
      ...this._def,
      checks: [...this._def.checks, check]
    });
  }
  int(message) {
    return this._addCheck({
      kind: "int",
      message: errorUtil.toString(message)
    });
  }
  positive(message) {
    return this._addCheck({
      kind: "min",
      value: 0,
      inclusive: false,
      message: errorUtil.toString(message)
    });
  }
  negative(message) {
    return this._addCheck({
      kind: "max",
      value: 0,
      inclusive: false,
      message: errorUtil.toString(message)
    });
  }
  nonpositive(message) {
    return this._addCheck({
      kind: "max",
      value: 0,
      inclusive: true,
      message: errorUtil.toString(message)
    });
  }
  nonnegative(message) {
    return this._addCheck({
      kind: "min",
      value: 0,
      inclusive: true,
      message: errorUtil.toString(message)
    });
  }
  multipleOf(value, message) {
    return this._addCheck({
      kind: "multipleOf",
      value,
      message: errorUtil.toString(message)
    });
  }
  finite(message) {
    return this._addCheck({
      kind: "finite",
      message: errorUtil.toString(message)
    });
  }
  safe(message) {
    return this._addCheck({
      kind: "min",
      inclusive: true,
      value: Number.MIN_SAFE_INTEGER,
      message: errorUtil.toString(message)
    })._addCheck({
      kind: "max",
      inclusive: true,
      value: Number.MAX_SAFE_INTEGER,
      message: errorUtil.toString(message)
    });
  }
  get minValue() {
    let min = null;
    for (const ch of this._def.checks) {
      if (ch.kind === "min") {
        if (min === null || ch.value > min)
          min = ch.value;
      }
    }
    return min;
  }
  get maxValue() {
    let max = null;
    for (const ch of this._def.checks) {
      if (ch.kind === "max") {
        if (max === null || ch.value < max)
          max = ch.value;
      }
    }
    return max;
  }
  get isInt() {
    return !!this._def.checks.find((ch) => ch.kind === "int" || ch.kind === "multipleOf" && util.isInteger(ch.value));
  }
  get isFinite() {
    let max = null;
    let min = null;
    for (const ch of this._def.checks) {
      if (ch.kind === "finite" || ch.kind === "int" || ch.kind === "multipleOf") {
        return true;
      } else if (ch.kind === "min") {
        if (min === null || ch.value > min)
          min = ch.value;
      } else if (ch.kind === "max") {
        if (max === null || ch.value < max)
          max = ch.value;
      }
    }
    return Number.isFinite(min) && Number.isFinite(max);
  }
};
ZodNumber.create = (params) => {
  return new ZodNumber({
    checks: [],
    typeName: ZodFirstPartyTypeKind.ZodNumber,
    coerce: params?.coerce || false,
    ...processCreateParams(params)
  });
};
var ZodBigInt = class _ZodBigInt extends ZodType {
  constructor() {
    super(...arguments);
    this.min = this.gte;
    this.max = this.lte;
  }
  _parse(input) {
    if (this._def.coerce) {
      try {
        input.data = BigInt(input.data);
      } catch {
        return this._getInvalidInput(input);
      }
    }
    const parsedType = this._getType(input);
    if (parsedType !== ZodParsedType.bigint) {
      return this._getInvalidInput(input);
    }
    let ctx = void 0;
    const status = new ParseStatus();
    for (const check of this._def.checks) {
      if (check.kind === "min") {
        const tooSmall = check.inclusive ? input.data < check.value : input.data <= check.value;
        if (tooSmall) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.too_small,
            type: "bigint",
            minimum: check.value,
            inclusive: check.inclusive,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "max") {
        const tooBig = check.inclusive ? input.data > check.value : input.data >= check.value;
        if (tooBig) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.too_big,
            type: "bigint",
            maximum: check.value,
            inclusive: check.inclusive,
            message: check.message
          });
          status.dirty();
        }
      } else if (check.kind === "multipleOf") {
        if (input.data % check.value !== BigInt(0)) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.not_multiple_of,
            multipleOf: check.value,
            message: check.message
          });
          status.dirty();
        }
      } else {
        util.assertNever(check);
      }
    }
    return { status: status.value, value: input.data };
  }
  _getInvalidInput(input) {
    const ctx = this._getOrReturnCtx(input);
    addIssueToContext(ctx, {
      code: ZodIssueCode.invalid_type,
      expected: ZodParsedType.bigint,
      received: ctx.parsedType
    });
    return INVALID;
  }
  gte(value, message) {
    return this.setLimit("min", value, true, errorUtil.toString(message));
  }
  gt(value, message) {
    return this.setLimit("min", value, false, errorUtil.toString(message));
  }
  lte(value, message) {
    return this.setLimit("max", value, true, errorUtil.toString(message));
  }
  lt(value, message) {
    return this.setLimit("max", value, false, errorUtil.toString(message));
  }
  setLimit(kind, value, inclusive, message) {
    return new _ZodBigInt({
      ...this._def,
      checks: [
        ...this._def.checks,
        {
          kind,
          value,
          inclusive,
          message: errorUtil.toString(message)
        }
      ]
    });
  }
  _addCheck(check) {
    return new _ZodBigInt({
      ...this._def,
      checks: [...this._def.checks, check]
    });
  }
  positive(message) {
    return this._addCheck({
      kind: "min",
      value: BigInt(0),
      inclusive: false,
      message: errorUtil.toString(message)
    });
  }
  negative(message) {
    return this._addCheck({
      kind: "max",
      value: BigInt(0),
      inclusive: false,
      message: errorUtil.toString(message)
    });
  }
  nonpositive(message) {
    return this._addCheck({
      kind: "max",
      value: BigInt(0),
      inclusive: true,
      message: errorUtil.toString(message)
    });
  }
  nonnegative(message) {
    return this._addCheck({
      kind: "min",
      value: BigInt(0),
      inclusive: true,
      message: errorUtil.toString(message)
    });
  }
  multipleOf(value, message) {
    return this._addCheck({
      kind: "multipleOf",
      value,
      message: errorUtil.toString(message)
    });
  }
  get minValue() {
    let min = null;
    for (const ch of this._def.checks) {
      if (ch.kind === "min") {
        if (min === null || ch.value > min)
          min = ch.value;
      }
    }
    return min;
  }
  get maxValue() {
    let max = null;
    for (const ch of this._def.checks) {
      if (ch.kind === "max") {
        if (max === null || ch.value < max)
          max = ch.value;
      }
    }
    return max;
  }
};
ZodBigInt.create = (params) => {
  return new ZodBigInt({
    checks: [],
    typeName: ZodFirstPartyTypeKind.ZodBigInt,
    coerce: params?.coerce ?? false,
    ...processCreateParams(params)
  });
};
var ZodBoolean = class extends ZodType {
  _parse(input) {
    if (this._def.coerce) {
      input.data = Boolean(input.data);
    }
    const parsedType = this._getType(input);
    if (parsedType !== ZodParsedType.boolean) {
      const ctx = this._getOrReturnCtx(input);
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.boolean,
        received: ctx.parsedType
      });
      return INVALID;
    }
    return OK(input.data);
  }
};
ZodBoolean.create = (params) => {
  return new ZodBoolean({
    typeName: ZodFirstPartyTypeKind.ZodBoolean,
    coerce: params?.coerce || false,
    ...processCreateParams(params)
  });
};
var ZodDate = class _ZodDate extends ZodType {
  _parse(input) {
    if (this._def.coerce) {
      input.data = new Date(input.data);
    }
    const parsedType = this._getType(input);
    if (parsedType !== ZodParsedType.date) {
      const ctx2 = this._getOrReturnCtx(input);
      addIssueToContext(ctx2, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.date,
        received: ctx2.parsedType
      });
      return INVALID;
    }
    if (Number.isNaN(input.data.getTime())) {
      const ctx2 = this._getOrReturnCtx(input);
      addIssueToContext(ctx2, {
        code: ZodIssueCode.invalid_date
      });
      return INVALID;
    }
    const status = new ParseStatus();
    let ctx = void 0;
    for (const check of this._def.checks) {
      if (check.kind === "min") {
        if (input.data.getTime() < check.value) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.too_small,
            message: check.message,
            inclusive: true,
            exact: false,
            minimum: check.value,
            type: "date"
          });
          status.dirty();
        }
      } else if (check.kind === "max") {
        if (input.data.getTime() > check.value) {
          ctx = this._getOrReturnCtx(input, ctx);
          addIssueToContext(ctx, {
            code: ZodIssueCode.too_big,
            message: check.message,
            inclusive: true,
            exact: false,
            maximum: check.value,
            type: "date"
          });
          status.dirty();
        }
      } else {
        util.assertNever(check);
      }
    }
    return {
      status: status.value,
      value: new Date(input.data.getTime())
    };
  }
  _addCheck(check) {
    return new _ZodDate({
      ...this._def,
      checks: [...this._def.checks, check]
    });
  }
  min(minDate, message) {
    return this._addCheck({
      kind: "min",
      value: minDate.getTime(),
      message: errorUtil.toString(message)
    });
  }
  max(maxDate, message) {
    return this._addCheck({
      kind: "max",
      value: maxDate.getTime(),
      message: errorUtil.toString(message)
    });
  }
  get minDate() {
    let min = null;
    for (const ch of this._def.checks) {
      if (ch.kind === "min") {
        if (min === null || ch.value > min)
          min = ch.value;
      }
    }
    return min != null ? new Date(min) : null;
  }
  get maxDate() {
    let max = null;
    for (const ch of this._def.checks) {
      if (ch.kind === "max") {
        if (max === null || ch.value < max)
          max = ch.value;
      }
    }
    return max != null ? new Date(max) : null;
  }
};
ZodDate.create = (params) => {
  return new ZodDate({
    checks: [],
    coerce: params?.coerce || false,
    typeName: ZodFirstPartyTypeKind.ZodDate,
    ...processCreateParams(params)
  });
};
var ZodSymbol = class extends ZodType {
  _parse(input) {
    const parsedType = this._getType(input);
    if (parsedType !== ZodParsedType.symbol) {
      const ctx = this._getOrReturnCtx(input);
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.symbol,
        received: ctx.parsedType
      });
      return INVALID;
    }
    return OK(input.data);
  }
};
ZodSymbol.create = (params) => {
  return new ZodSymbol({
    typeName: ZodFirstPartyTypeKind.ZodSymbol,
    ...processCreateParams(params)
  });
};
var ZodUndefined = class extends ZodType {
  _parse(input) {
    const parsedType = this._getType(input);
    if (parsedType !== ZodParsedType.undefined) {
      const ctx = this._getOrReturnCtx(input);
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.undefined,
        received: ctx.parsedType
      });
      return INVALID;
    }
    return OK(input.data);
  }
};
ZodUndefined.create = (params) => {
  return new ZodUndefined({
    typeName: ZodFirstPartyTypeKind.ZodUndefined,
    ...processCreateParams(params)
  });
};
var ZodNull = class extends ZodType {
  _parse(input) {
    const parsedType = this._getType(input);
    if (parsedType !== ZodParsedType.null) {
      const ctx = this._getOrReturnCtx(input);
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.null,
        received: ctx.parsedType
      });
      return INVALID;
    }
    return OK(input.data);
  }
};
ZodNull.create = (params) => {
  return new ZodNull({
    typeName: ZodFirstPartyTypeKind.ZodNull,
    ...processCreateParams(params)
  });
};
var ZodAny = class extends ZodType {
  constructor() {
    super(...arguments);
    this._any = true;
  }
  _parse(input) {
    return OK(input.data);
  }
};
ZodAny.create = (params) => {
  return new ZodAny({
    typeName: ZodFirstPartyTypeKind.ZodAny,
    ...processCreateParams(params)
  });
};
var ZodUnknown = class extends ZodType {
  constructor() {
    super(...arguments);
    this._unknown = true;
  }
  _parse(input) {
    return OK(input.data);
  }
};
ZodUnknown.create = (params) => {
  return new ZodUnknown({
    typeName: ZodFirstPartyTypeKind.ZodUnknown,
    ...processCreateParams(params)
  });
};
var ZodNever = class extends ZodType {
  _parse(input) {
    const ctx = this._getOrReturnCtx(input);
    addIssueToContext(ctx, {
      code: ZodIssueCode.invalid_type,
      expected: ZodParsedType.never,
      received: ctx.parsedType
    });
    return INVALID;
  }
};
ZodNever.create = (params) => {
  return new ZodNever({
    typeName: ZodFirstPartyTypeKind.ZodNever,
    ...processCreateParams(params)
  });
};
var ZodVoid = class extends ZodType {
  _parse(input) {
    const parsedType = this._getType(input);
    if (parsedType !== ZodParsedType.undefined) {
      const ctx = this._getOrReturnCtx(input);
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.void,
        received: ctx.parsedType
      });
      return INVALID;
    }
    return OK(input.data);
  }
};
ZodVoid.create = (params) => {
  return new ZodVoid({
    typeName: ZodFirstPartyTypeKind.ZodVoid,
    ...processCreateParams(params)
  });
};
var ZodArray = class _ZodArray extends ZodType {
  _parse(input) {
    const { ctx, status } = this._processInputParams(input);
    const def = this._def;
    if (ctx.parsedType !== ZodParsedType.array) {
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.array,
        received: ctx.parsedType
      });
      return INVALID;
    }
    if (def.exactLength !== null) {
      const tooBig = ctx.data.length > def.exactLength.value;
      const tooSmall = ctx.data.length < def.exactLength.value;
      if (tooBig || tooSmall) {
        addIssueToContext(ctx, {
          code: tooBig ? ZodIssueCode.too_big : ZodIssueCode.too_small,
          minimum: tooSmall ? def.exactLength.value : void 0,
          maximum: tooBig ? def.exactLength.value : void 0,
          type: "array",
          inclusive: true,
          exact: true,
          message: def.exactLength.message
        });
        status.dirty();
      }
    }
    if (def.minLength !== null) {
      if (ctx.data.length < def.minLength.value) {
        addIssueToContext(ctx, {
          code: ZodIssueCode.too_small,
          minimum: def.minLength.value,
          type: "array",
          inclusive: true,
          exact: false,
          message: def.minLength.message
        });
        status.dirty();
      }
    }
    if (def.maxLength !== null) {
      if (ctx.data.length > def.maxLength.value) {
        addIssueToContext(ctx, {
          code: ZodIssueCode.too_big,
          maximum: def.maxLength.value,
          type: "array",
          inclusive: true,
          exact: false,
          message: def.maxLength.message
        });
        status.dirty();
      }
    }
    if (ctx.common.async) {
      return Promise.all([...ctx.data].map((item, i7) => {
        return def.type._parseAsync(new ParseInputLazyPath(ctx, item, ctx.path, i7));
      })).then((result2) => {
        return ParseStatus.mergeArray(status, result2);
      });
    }
    const result = [...ctx.data].map((item, i7) => {
      return def.type._parseSync(new ParseInputLazyPath(ctx, item, ctx.path, i7));
    });
    return ParseStatus.mergeArray(status, result);
  }
  get element() {
    return this._def.type;
  }
  min(minLength, message) {
    return new _ZodArray({
      ...this._def,
      minLength: { value: minLength, message: errorUtil.toString(message) }
    });
  }
  max(maxLength, message) {
    return new _ZodArray({
      ...this._def,
      maxLength: { value: maxLength, message: errorUtil.toString(message) }
    });
  }
  length(len, message) {
    return new _ZodArray({
      ...this._def,
      exactLength: { value: len, message: errorUtil.toString(message) }
    });
  }
  nonempty(message) {
    return this.min(1, message);
  }
};
ZodArray.create = (schema, params) => {
  return new ZodArray({
    type: schema,
    minLength: null,
    maxLength: null,
    exactLength: null,
    typeName: ZodFirstPartyTypeKind.ZodArray,
    ...processCreateParams(params)
  });
};
function deepPartialify(schema) {
  if (schema instanceof ZodObject) {
    const newShape = {};
    for (const key in schema.shape) {
      const fieldSchema = schema.shape[key];
      newShape[key] = ZodOptional.create(deepPartialify(fieldSchema));
    }
    return new ZodObject({
      ...schema._def,
      shape: () => newShape
    });
  } else if (schema instanceof ZodArray) {
    return new ZodArray({
      ...schema._def,
      type: deepPartialify(schema.element)
    });
  } else if (schema instanceof ZodOptional) {
    return ZodOptional.create(deepPartialify(schema.unwrap()));
  } else if (schema instanceof ZodNullable) {
    return ZodNullable.create(deepPartialify(schema.unwrap()));
  } else if (schema instanceof ZodTuple) {
    return ZodTuple.create(schema.items.map((item) => deepPartialify(item)));
  } else {
    return schema;
  }
}
var ZodObject = class _ZodObject extends ZodType {
  constructor() {
    super(...arguments);
    this._cached = null;
    this.nonstrict = this.passthrough;
    this.augment = this.extend;
  }
  _getCached() {
    if (this._cached !== null)
      return this._cached;
    const shape = this._def.shape();
    const keys = util.objectKeys(shape);
    this._cached = { shape, keys };
    return this._cached;
  }
  _parse(input) {
    const parsedType = this._getType(input);
    if (parsedType !== ZodParsedType.object) {
      const ctx2 = this._getOrReturnCtx(input);
      addIssueToContext(ctx2, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.object,
        received: ctx2.parsedType
      });
      return INVALID;
    }
    const { status, ctx } = this._processInputParams(input);
    const { shape, keys: shapeKeys } = this._getCached();
    const extraKeys = [];
    if (!(this._def.catchall instanceof ZodNever && this._def.unknownKeys === "strip")) {
      for (const key in ctx.data) {
        if (!shapeKeys.includes(key)) {
          extraKeys.push(key);
        }
      }
    }
    const pairs = [];
    for (const key of shapeKeys) {
      const keyValidator = shape[key];
      const value = ctx.data[key];
      pairs.push({
        key: { status: "valid", value: key },
        value: keyValidator._parse(new ParseInputLazyPath(ctx, value, ctx.path, key)),
        alwaysSet: key in ctx.data
      });
    }
    if (this._def.catchall instanceof ZodNever) {
      const unknownKeys = this._def.unknownKeys;
      if (unknownKeys === "passthrough") {
        for (const key of extraKeys) {
          pairs.push({
            key: { status: "valid", value: key },
            value: { status: "valid", value: ctx.data[key] }
          });
        }
      } else if (unknownKeys === "strict") {
        if (extraKeys.length > 0) {
          addIssueToContext(ctx, {
            code: ZodIssueCode.unrecognized_keys,
            keys: extraKeys
          });
          status.dirty();
        }
      } else if (unknownKeys === "strip") {
      } else {
        throw new Error(`Internal ZodObject error: invalid unknownKeys value.`);
      }
    } else {
      const catchall = this._def.catchall;
      for (const key of extraKeys) {
        const value = ctx.data[key];
        pairs.push({
          key: { status: "valid", value: key },
          value: catchall._parse(
            new ParseInputLazyPath(ctx, value, ctx.path, key)
            //, ctx.child(key), value, getParsedType(value)
          ),
          alwaysSet: key in ctx.data
        });
      }
    }
    if (ctx.common.async) {
      return Promise.resolve().then(async () => {
        const syncPairs = [];
        for (const pair of pairs) {
          const key = await pair.key;
          const value = await pair.value;
          syncPairs.push({
            key,
            value,
            alwaysSet: pair.alwaysSet
          });
        }
        return syncPairs;
      }).then((syncPairs) => {
        return ParseStatus.mergeObjectSync(status, syncPairs);
      });
    } else {
      return ParseStatus.mergeObjectSync(status, pairs);
    }
  }
  get shape() {
    return this._def.shape();
  }
  strict(message) {
    errorUtil.errToObj;
    return new _ZodObject({
      ...this._def,
      unknownKeys: "strict",
      ...message !== void 0 ? {
        errorMap: (issue, ctx) => {
          const defaultError = this._def.errorMap?.(issue, ctx).message ?? ctx.defaultError;
          if (issue.code === "unrecognized_keys")
            return {
              message: errorUtil.errToObj(message).message ?? defaultError
            };
          return {
            message: defaultError
          };
        }
      } : {}
    });
  }
  strip() {
    return new _ZodObject({
      ...this._def,
      unknownKeys: "strip"
    });
  }
  passthrough() {
    return new _ZodObject({
      ...this._def,
      unknownKeys: "passthrough"
    });
  }
  // const AugmentFactory =
  //   <Def extends ZodObjectDef>(def: Def) =>
  //   <Augmentation extends ZodRawShape>(
  //     augmentation: Augmentation
  //   ): ZodObject<
  //     extendShape<ReturnType<Def["shape"]>, Augmentation>,
  //     Def["unknownKeys"],
  //     Def["catchall"]
  //   > => {
  //     return new ZodObject({
  //       ...def,
  //       shape: () => ({
  //         ...def.shape(),
  //         ...augmentation,
  //       }),
  //     }) as any;
  //   };
  extend(augmentation) {
    return new _ZodObject({
      ...this._def,
      shape: () => ({
        ...this._def.shape(),
        ...augmentation
      })
    });
  }
  /**
   * Prior to zod@1.0.12 there was a bug in the
   * inferred type of merged objects. Please
   * upgrade if you are experiencing issues.
   */
  merge(merging) {
    const merged = new _ZodObject({
      unknownKeys: merging._def.unknownKeys,
      catchall: merging._def.catchall,
      shape: () => ({
        ...this._def.shape(),
        ...merging._def.shape()
      }),
      typeName: ZodFirstPartyTypeKind.ZodObject
    });
    return merged;
  }
  // merge<
  //   Incoming extends AnyZodObject,
  //   Augmentation extends Incoming["shape"],
  //   NewOutput extends {
  //     [k in keyof Augmentation | keyof Output]: k extends keyof Augmentation
  //       ? Augmentation[k]["_output"]
  //       : k extends keyof Output
  //       ? Output[k]
  //       : never;
  //   },
  //   NewInput extends {
  //     [k in keyof Augmentation | keyof Input]: k extends keyof Augmentation
  //       ? Augmentation[k]["_input"]
  //       : k extends keyof Input
  //       ? Input[k]
  //       : never;
  //   }
  // >(
  //   merging: Incoming
  // ): ZodObject<
  //   extendShape<T, ReturnType<Incoming["_def"]["shape"]>>,
  //   Incoming["_def"]["unknownKeys"],
  //   Incoming["_def"]["catchall"],
  //   NewOutput,
  //   NewInput
  // > {
  //   const merged: any = new ZodObject({
  //     unknownKeys: merging._def.unknownKeys,
  //     catchall: merging._def.catchall,
  //     shape: () =>
  //       objectUtil.mergeShapes(this._def.shape(), merging._def.shape()),
  //     typeName: ZodFirstPartyTypeKind.ZodObject,
  //   }) as any;
  //   return merged;
  // }
  setKey(key, schema) {
    return this.augment({ [key]: schema });
  }
  // merge<Incoming extends AnyZodObject>(
  //   merging: Incoming
  // ): //ZodObject<T & Incoming["_shape"], UnknownKeys, Catchall> = (merging) => {
  // ZodObject<
  //   extendShape<T, ReturnType<Incoming["_def"]["shape"]>>,
  //   Incoming["_def"]["unknownKeys"],
  //   Incoming["_def"]["catchall"]
  // > {
  //   // const mergedShape = objectUtil.mergeShapes(
  //   //   this._def.shape(),
  //   //   merging._def.shape()
  //   // );
  //   const merged: any = new ZodObject({
  //     unknownKeys: merging._def.unknownKeys,
  //     catchall: merging._def.catchall,
  //     shape: () =>
  //       objectUtil.mergeShapes(this._def.shape(), merging._def.shape()),
  //     typeName: ZodFirstPartyTypeKind.ZodObject,
  //   }) as any;
  //   return merged;
  // }
  catchall(index) {
    return new _ZodObject({
      ...this._def,
      catchall: index
    });
  }
  pick(mask) {
    const shape = {};
    for (const key of util.objectKeys(mask)) {
      if (mask[key] && this.shape[key]) {
        shape[key] = this.shape[key];
      }
    }
    return new _ZodObject({
      ...this._def,
      shape: () => shape
    });
  }
  omit(mask) {
    const shape = {};
    for (const key of util.objectKeys(this.shape)) {
      if (!mask[key]) {
        shape[key] = this.shape[key];
      }
    }
    return new _ZodObject({
      ...this._def,
      shape: () => shape
    });
  }
  /**
   * @deprecated
   */
  deepPartial() {
    return deepPartialify(this);
  }
  partial(mask) {
    const newShape = {};
    for (const key of util.objectKeys(this.shape)) {
      const fieldSchema = this.shape[key];
      if (mask && !mask[key]) {
        newShape[key] = fieldSchema;
      } else {
        newShape[key] = fieldSchema.optional();
      }
    }
    return new _ZodObject({
      ...this._def,
      shape: () => newShape
    });
  }
  required(mask) {
    const newShape = {};
    for (const key of util.objectKeys(this.shape)) {
      if (mask && !mask[key]) {
        newShape[key] = this.shape[key];
      } else {
        const fieldSchema = this.shape[key];
        let newField = fieldSchema;
        while (newField instanceof ZodOptional) {
          newField = newField._def.innerType;
        }
        newShape[key] = newField;
      }
    }
    return new _ZodObject({
      ...this._def,
      shape: () => newShape
    });
  }
  keyof() {
    return createZodEnum(util.objectKeys(this.shape));
  }
};
ZodObject.create = (shape, params) => {
  return new ZodObject({
    shape: () => shape,
    unknownKeys: "strip",
    catchall: ZodNever.create(),
    typeName: ZodFirstPartyTypeKind.ZodObject,
    ...processCreateParams(params)
  });
};
ZodObject.strictCreate = (shape, params) => {
  return new ZodObject({
    shape: () => shape,
    unknownKeys: "strict",
    catchall: ZodNever.create(),
    typeName: ZodFirstPartyTypeKind.ZodObject,
    ...processCreateParams(params)
  });
};
ZodObject.lazycreate = (shape, params) => {
  return new ZodObject({
    shape,
    unknownKeys: "strip",
    catchall: ZodNever.create(),
    typeName: ZodFirstPartyTypeKind.ZodObject,
    ...processCreateParams(params)
  });
};
var ZodUnion = class extends ZodType {
  _parse(input) {
    const { ctx } = this._processInputParams(input);
    const options = this._def.options;
    function handleResults(results) {
      for (const result of results) {
        if (result.result.status === "valid") {
          return result.result;
        }
      }
      for (const result of results) {
        if (result.result.status === "dirty") {
          ctx.common.issues.push(...result.ctx.common.issues);
          return result.result;
        }
      }
      const unionErrors = results.map((result) => new ZodError(result.ctx.common.issues));
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_union,
        unionErrors
      });
      return INVALID;
    }
    if (ctx.common.async) {
      return Promise.all(options.map(async (option) => {
        const childCtx = {
          ...ctx,
          common: {
            ...ctx.common,
            issues: []
          },
          parent: null
        };
        return {
          result: await option._parseAsync({
            data: ctx.data,
            path: ctx.path,
            parent: childCtx
          }),
          ctx: childCtx
        };
      })).then(handleResults);
    } else {
      let dirty = void 0;
      const issues = [];
      for (const option of options) {
        const childCtx = {
          ...ctx,
          common: {
            ...ctx.common,
            issues: []
          },
          parent: null
        };
        const result = option._parseSync({
          data: ctx.data,
          path: ctx.path,
          parent: childCtx
        });
        if (result.status === "valid") {
          return result;
        } else if (result.status === "dirty" && !dirty) {
          dirty = { result, ctx: childCtx };
        }
        if (childCtx.common.issues.length) {
          issues.push(childCtx.common.issues);
        }
      }
      if (dirty) {
        ctx.common.issues.push(...dirty.ctx.common.issues);
        return dirty.result;
      }
      const unionErrors = issues.map((issues2) => new ZodError(issues2));
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_union,
        unionErrors
      });
      return INVALID;
    }
  }
  get options() {
    return this._def.options;
  }
};
ZodUnion.create = (types, params) => {
  return new ZodUnion({
    options: types,
    typeName: ZodFirstPartyTypeKind.ZodUnion,
    ...processCreateParams(params)
  });
};
var getDiscriminator = (type) => {
  if (type instanceof ZodLazy) {
    return getDiscriminator(type.schema);
  } else if (type instanceof ZodEffects) {
    return getDiscriminator(type.innerType());
  } else if (type instanceof ZodLiteral) {
    return [type.value];
  } else if (type instanceof ZodEnum) {
    return type.options;
  } else if (type instanceof ZodNativeEnum) {
    return util.objectValues(type.enum);
  } else if (type instanceof ZodDefault) {
    return getDiscriminator(type._def.innerType);
  } else if (type instanceof ZodUndefined) {
    return [void 0];
  } else if (type instanceof ZodNull) {
    return [null];
  } else if (type instanceof ZodOptional) {
    return [void 0, ...getDiscriminator(type.unwrap())];
  } else if (type instanceof ZodNullable) {
    return [null, ...getDiscriminator(type.unwrap())];
  } else if (type instanceof ZodBranded) {
    return getDiscriminator(type.unwrap());
  } else if (type instanceof ZodReadonly) {
    return getDiscriminator(type.unwrap());
  } else if (type instanceof ZodCatch) {
    return getDiscriminator(type._def.innerType);
  } else {
    return [];
  }
};
var ZodDiscriminatedUnion = class _ZodDiscriminatedUnion extends ZodType {
  _parse(input) {
    const { ctx } = this._processInputParams(input);
    if (ctx.parsedType !== ZodParsedType.object) {
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.object,
        received: ctx.parsedType
      });
      return INVALID;
    }
    const discriminator = this.discriminator;
    const discriminatorValue = ctx.data[discriminator];
    const option = this.optionsMap.get(discriminatorValue);
    if (!option) {
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_union_discriminator,
        options: Array.from(this.optionsMap.keys()),
        path: [discriminator]
      });
      return INVALID;
    }
    if (ctx.common.async) {
      return option._parseAsync({
        data: ctx.data,
        path: ctx.path,
        parent: ctx
      });
    } else {
      return option._parseSync({
        data: ctx.data,
        path: ctx.path,
        parent: ctx
      });
    }
  }
  get discriminator() {
    return this._def.discriminator;
  }
  get options() {
    return this._def.options;
  }
  get optionsMap() {
    return this._def.optionsMap;
  }
  /**
   * The constructor of the discriminated union schema. Its behaviour is very similar to that of the normal z.union() constructor.
   * However, it only allows a union of objects, all of which need to share a discriminator property. This property must
   * have a different value for each object in the union.
   * @param discriminator the name of the discriminator property
   * @param types an array of object schemas
   * @param params
   */
  static create(discriminator, options, params) {
    const optionsMap = /* @__PURE__ */ new Map();
    for (const type of options) {
      const discriminatorValues = getDiscriminator(type.shape[discriminator]);
      if (!discriminatorValues.length) {
        throw new Error(`A discriminator value for key \`${discriminator}\` could not be extracted from all schema options`);
      }
      for (const value of discriminatorValues) {
        if (optionsMap.has(value)) {
          throw new Error(`Discriminator property ${String(discriminator)} has duplicate value ${String(value)}`);
        }
        optionsMap.set(value, type);
      }
    }
    return new _ZodDiscriminatedUnion({
      typeName: ZodFirstPartyTypeKind.ZodDiscriminatedUnion,
      discriminator,
      options,
      optionsMap,
      ...processCreateParams(params)
    });
  }
};
function mergeValues(a4, b4) {
  const aType = getParsedType(a4);
  const bType = getParsedType(b4);
  if (a4 === b4) {
    return { valid: true, data: a4 };
  } else if (aType === ZodParsedType.object && bType === ZodParsedType.object) {
    const bKeys = util.objectKeys(b4);
    const sharedKeys = util.objectKeys(a4).filter((key) => bKeys.indexOf(key) !== -1);
    const newObj = { ...a4, ...b4 };
    for (const key of sharedKeys) {
      const sharedValue = mergeValues(a4[key], b4[key]);
      if (!sharedValue.valid) {
        return { valid: false };
      }
      newObj[key] = sharedValue.data;
    }
    return { valid: true, data: newObj };
  } else if (aType === ZodParsedType.array && bType === ZodParsedType.array) {
    if (a4.length !== b4.length) {
      return { valid: false };
    }
    const newArray = [];
    for (let index = 0; index < a4.length; index++) {
      const itemA = a4[index];
      const itemB = b4[index];
      const sharedValue = mergeValues(itemA, itemB);
      if (!sharedValue.valid) {
        return { valid: false };
      }
      newArray.push(sharedValue.data);
    }
    return { valid: true, data: newArray };
  } else if (aType === ZodParsedType.date && bType === ZodParsedType.date && +a4 === +b4) {
    return { valid: true, data: a4 };
  } else {
    return { valid: false };
  }
}
var ZodIntersection = class extends ZodType {
  _parse(input) {
    const { status, ctx } = this._processInputParams(input);
    const handleParsed = (parsedLeft, parsedRight) => {
      if (isAborted(parsedLeft) || isAborted(parsedRight)) {
        return INVALID;
      }
      const merged = mergeValues(parsedLeft.value, parsedRight.value);
      if (!merged.valid) {
        addIssueToContext(ctx, {
          code: ZodIssueCode.invalid_intersection_types
        });
        return INVALID;
      }
      if (isDirty(parsedLeft) || isDirty(parsedRight)) {
        status.dirty();
      }
      return { status: status.value, value: merged.data };
    };
    if (ctx.common.async) {
      return Promise.all([
        this._def.left._parseAsync({
          data: ctx.data,
          path: ctx.path,
          parent: ctx
        }),
        this._def.right._parseAsync({
          data: ctx.data,
          path: ctx.path,
          parent: ctx
        })
      ]).then(([left, right]) => handleParsed(left, right));
    } else {
      return handleParsed(this._def.left._parseSync({
        data: ctx.data,
        path: ctx.path,
        parent: ctx
      }), this._def.right._parseSync({
        data: ctx.data,
        path: ctx.path,
        parent: ctx
      }));
    }
  }
};
ZodIntersection.create = (left, right, params) => {
  return new ZodIntersection({
    left,
    right,
    typeName: ZodFirstPartyTypeKind.ZodIntersection,
    ...processCreateParams(params)
  });
};
var ZodTuple = class _ZodTuple extends ZodType {
  _parse(input) {
    const { status, ctx } = this._processInputParams(input);
    if (ctx.parsedType !== ZodParsedType.array) {
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.array,
        received: ctx.parsedType
      });
      return INVALID;
    }
    if (ctx.data.length < this._def.items.length) {
      addIssueToContext(ctx, {
        code: ZodIssueCode.too_small,
        minimum: this._def.items.length,
        inclusive: true,
        exact: false,
        type: "array"
      });
      return INVALID;
    }
    const rest = this._def.rest;
    if (!rest && ctx.data.length > this._def.items.length) {
      addIssueToContext(ctx, {
        code: ZodIssueCode.too_big,
        maximum: this._def.items.length,
        inclusive: true,
        exact: false,
        type: "array"
      });
      status.dirty();
    }
    const items = [...ctx.data].map((item, itemIndex) => {
      const schema = this._def.items[itemIndex] || this._def.rest;
      if (!schema)
        return null;
      return schema._parse(new ParseInputLazyPath(ctx, item, ctx.path, itemIndex));
    }).filter((x3) => !!x3);
    if (ctx.common.async) {
      return Promise.all(items).then((results) => {
        return ParseStatus.mergeArray(status, results);
      });
    } else {
      return ParseStatus.mergeArray(status, items);
    }
  }
  get items() {
    return this._def.items;
  }
  rest(rest) {
    return new _ZodTuple({
      ...this._def,
      rest
    });
  }
};
ZodTuple.create = (schemas, params) => {
  if (!Array.isArray(schemas)) {
    throw new Error("You must pass an array of schemas to z.tuple([ ... ])");
  }
  return new ZodTuple({
    items: schemas,
    typeName: ZodFirstPartyTypeKind.ZodTuple,
    rest: null,
    ...processCreateParams(params)
  });
};
var ZodRecord = class _ZodRecord extends ZodType {
  get keySchema() {
    return this._def.keyType;
  }
  get valueSchema() {
    return this._def.valueType;
  }
  _parse(input) {
    const { status, ctx } = this._processInputParams(input);
    if (ctx.parsedType !== ZodParsedType.object) {
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.object,
        received: ctx.parsedType
      });
      return INVALID;
    }
    const pairs = [];
    const keyType = this._def.keyType;
    const valueType = this._def.valueType;
    for (const key in ctx.data) {
      pairs.push({
        key: keyType._parse(new ParseInputLazyPath(ctx, key, ctx.path, key)),
        value: valueType._parse(new ParseInputLazyPath(ctx, ctx.data[key], ctx.path, key)),
        alwaysSet: key in ctx.data
      });
    }
    if (ctx.common.async) {
      return ParseStatus.mergeObjectAsync(status, pairs);
    } else {
      return ParseStatus.mergeObjectSync(status, pairs);
    }
  }
  get element() {
    return this._def.valueType;
  }
  static create(first, second, third) {
    if (second instanceof ZodType) {
      return new _ZodRecord({
        keyType: first,
        valueType: second,
        typeName: ZodFirstPartyTypeKind.ZodRecord,
        ...processCreateParams(third)
      });
    }
    return new _ZodRecord({
      keyType: ZodString.create(),
      valueType: first,
      typeName: ZodFirstPartyTypeKind.ZodRecord,
      ...processCreateParams(second)
    });
  }
};
var ZodMap = class extends ZodType {
  get keySchema() {
    return this._def.keyType;
  }
  get valueSchema() {
    return this._def.valueType;
  }
  _parse(input) {
    const { status, ctx } = this._processInputParams(input);
    if (ctx.parsedType !== ZodParsedType.map) {
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.map,
        received: ctx.parsedType
      });
      return INVALID;
    }
    const keyType = this._def.keyType;
    const valueType = this._def.valueType;
    const pairs = [...ctx.data.entries()].map(([key, value], index) => {
      return {
        key: keyType._parse(new ParseInputLazyPath(ctx, key, ctx.path, [index, "key"])),
        value: valueType._parse(new ParseInputLazyPath(ctx, value, ctx.path, [index, "value"]))
      };
    });
    if (ctx.common.async) {
      const finalMap = /* @__PURE__ */ new Map();
      return Promise.resolve().then(async () => {
        for (const pair of pairs) {
          const key = await pair.key;
          const value = await pair.value;
          if (key.status === "aborted" || value.status === "aborted") {
            return INVALID;
          }
          if (key.status === "dirty" || value.status === "dirty") {
            status.dirty();
          }
          finalMap.set(key.value, value.value);
        }
        return { status: status.value, value: finalMap };
      });
    } else {
      const finalMap = /* @__PURE__ */ new Map();
      for (const pair of pairs) {
        const key = pair.key;
        const value = pair.value;
        if (key.status === "aborted" || value.status === "aborted") {
          return INVALID;
        }
        if (key.status === "dirty" || value.status === "dirty") {
          status.dirty();
        }
        finalMap.set(key.value, value.value);
      }
      return { status: status.value, value: finalMap };
    }
  }
};
ZodMap.create = (keyType, valueType, params) => {
  return new ZodMap({
    valueType,
    keyType,
    typeName: ZodFirstPartyTypeKind.ZodMap,
    ...processCreateParams(params)
  });
};
var ZodSet = class _ZodSet extends ZodType {
  _parse(input) {
    const { status, ctx } = this._processInputParams(input);
    if (ctx.parsedType !== ZodParsedType.set) {
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.set,
        received: ctx.parsedType
      });
      return INVALID;
    }
    const def = this._def;
    if (def.minSize !== null) {
      if (ctx.data.size < def.minSize.value) {
        addIssueToContext(ctx, {
          code: ZodIssueCode.too_small,
          minimum: def.minSize.value,
          type: "set",
          inclusive: true,
          exact: false,
          message: def.minSize.message
        });
        status.dirty();
      }
    }
    if (def.maxSize !== null) {
      if (ctx.data.size > def.maxSize.value) {
        addIssueToContext(ctx, {
          code: ZodIssueCode.too_big,
          maximum: def.maxSize.value,
          type: "set",
          inclusive: true,
          exact: false,
          message: def.maxSize.message
        });
        status.dirty();
      }
    }
    const valueType = this._def.valueType;
    function finalizeSet(elements2) {
      const parsedSet = /* @__PURE__ */ new Set();
      for (const element of elements2) {
        if (element.status === "aborted")
          return INVALID;
        if (element.status === "dirty")
          status.dirty();
        parsedSet.add(element.value);
      }
      return { status: status.value, value: parsedSet };
    }
    const elements = [...ctx.data.values()].map((item, i7) => valueType._parse(new ParseInputLazyPath(ctx, item, ctx.path, i7)));
    if (ctx.common.async) {
      return Promise.all(elements).then((elements2) => finalizeSet(elements2));
    } else {
      return finalizeSet(elements);
    }
  }
  min(minSize, message) {
    return new _ZodSet({
      ...this._def,
      minSize: { value: minSize, message: errorUtil.toString(message) }
    });
  }
  max(maxSize, message) {
    return new _ZodSet({
      ...this._def,
      maxSize: { value: maxSize, message: errorUtil.toString(message) }
    });
  }
  size(size, message) {
    return this.min(size, message).max(size, message);
  }
  nonempty(message) {
    return this.min(1, message);
  }
};
ZodSet.create = (valueType, params) => {
  return new ZodSet({
    valueType,
    minSize: null,
    maxSize: null,
    typeName: ZodFirstPartyTypeKind.ZodSet,
    ...processCreateParams(params)
  });
};
var ZodFunction = class _ZodFunction extends ZodType {
  constructor() {
    super(...arguments);
    this.validate = this.implement;
  }
  _parse(input) {
    const { ctx } = this._processInputParams(input);
    if (ctx.parsedType !== ZodParsedType.function) {
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.function,
        received: ctx.parsedType
      });
      return INVALID;
    }
    function makeArgsIssue(args, error) {
      return makeIssue({
        data: args,
        path: ctx.path,
        errorMaps: [ctx.common.contextualErrorMap, ctx.schemaErrorMap, getErrorMap(), en_default].filter((x3) => !!x3),
        issueData: {
          code: ZodIssueCode.invalid_arguments,
          argumentsError: error
        }
      });
    }
    function makeReturnsIssue(returns, error) {
      return makeIssue({
        data: returns,
        path: ctx.path,
        errorMaps: [ctx.common.contextualErrorMap, ctx.schemaErrorMap, getErrorMap(), en_default].filter((x3) => !!x3),
        issueData: {
          code: ZodIssueCode.invalid_return_type,
          returnTypeError: error
        }
      });
    }
    const params = { errorMap: ctx.common.contextualErrorMap };
    const fn = ctx.data;
    if (this._def.returns instanceof ZodPromise) {
      const me = this;
      return OK(async function(...args) {
        const error = new ZodError([]);
        const parsedArgs = await me._def.args.parseAsync(args, params).catch((e7) => {
          error.addIssue(makeArgsIssue(args, e7));
          throw error;
        });
        const result = await Reflect.apply(fn, this, parsedArgs);
        const parsedReturns = await me._def.returns._def.type.parseAsync(result, params).catch((e7) => {
          error.addIssue(makeReturnsIssue(result, e7));
          throw error;
        });
        return parsedReturns;
      });
    } else {
      const me = this;
      return OK(function(...args) {
        const parsedArgs = me._def.args.safeParse(args, params);
        if (!parsedArgs.success) {
          throw new ZodError([makeArgsIssue(args, parsedArgs.error)]);
        }
        const result = Reflect.apply(fn, this, parsedArgs.data);
        const parsedReturns = me._def.returns.safeParse(result, params);
        if (!parsedReturns.success) {
          throw new ZodError([makeReturnsIssue(result, parsedReturns.error)]);
        }
        return parsedReturns.data;
      });
    }
  }
  parameters() {
    return this._def.args;
  }
  returnType() {
    return this._def.returns;
  }
  args(...items) {
    return new _ZodFunction({
      ...this._def,
      args: ZodTuple.create(items).rest(ZodUnknown.create())
    });
  }
  returns(returnType) {
    return new _ZodFunction({
      ...this._def,
      returns: returnType
    });
  }
  implement(func) {
    const validatedFunc = this.parse(func);
    return validatedFunc;
  }
  strictImplement(func) {
    const validatedFunc = this.parse(func);
    return validatedFunc;
  }
  static create(args, returns, params) {
    return new _ZodFunction({
      args: args ? args : ZodTuple.create([]).rest(ZodUnknown.create()),
      returns: returns || ZodUnknown.create(),
      typeName: ZodFirstPartyTypeKind.ZodFunction,
      ...processCreateParams(params)
    });
  }
};
var ZodLazy = class extends ZodType {
  get schema() {
    return this._def.getter();
  }
  _parse(input) {
    const { ctx } = this._processInputParams(input);
    const lazySchema = this._def.getter();
    return lazySchema._parse({ data: ctx.data, path: ctx.path, parent: ctx });
  }
};
ZodLazy.create = (getter, params) => {
  return new ZodLazy({
    getter,
    typeName: ZodFirstPartyTypeKind.ZodLazy,
    ...processCreateParams(params)
  });
};
var ZodLiteral = class extends ZodType {
  _parse(input) {
    if (input.data !== this._def.value) {
      const ctx = this._getOrReturnCtx(input);
      addIssueToContext(ctx, {
        received: ctx.data,
        code: ZodIssueCode.invalid_literal,
        expected: this._def.value
      });
      return INVALID;
    }
    return { status: "valid", value: input.data };
  }
  get value() {
    return this._def.value;
  }
};
ZodLiteral.create = (value, params) => {
  return new ZodLiteral({
    value,
    typeName: ZodFirstPartyTypeKind.ZodLiteral,
    ...processCreateParams(params)
  });
};
function createZodEnum(values, params) {
  return new ZodEnum({
    values,
    typeName: ZodFirstPartyTypeKind.ZodEnum,
    ...processCreateParams(params)
  });
}
var ZodEnum = class _ZodEnum extends ZodType {
  _parse(input) {
    if (typeof input.data !== "string") {
      const ctx = this._getOrReturnCtx(input);
      const expectedValues = this._def.values;
      addIssueToContext(ctx, {
        expected: util.joinValues(expectedValues),
        received: ctx.parsedType,
        code: ZodIssueCode.invalid_type
      });
      return INVALID;
    }
    if (!this._cache) {
      this._cache = new Set(this._def.values);
    }
    if (!this._cache.has(input.data)) {
      const ctx = this._getOrReturnCtx(input);
      const expectedValues = this._def.values;
      addIssueToContext(ctx, {
        received: ctx.data,
        code: ZodIssueCode.invalid_enum_value,
        options: expectedValues
      });
      return INVALID;
    }
    return OK(input.data);
  }
  get options() {
    return this._def.values;
  }
  get enum() {
    const enumValues = {};
    for (const val of this._def.values) {
      enumValues[val] = val;
    }
    return enumValues;
  }
  get Values() {
    const enumValues = {};
    for (const val of this._def.values) {
      enumValues[val] = val;
    }
    return enumValues;
  }
  get Enum() {
    const enumValues = {};
    for (const val of this._def.values) {
      enumValues[val] = val;
    }
    return enumValues;
  }
  extract(values, newDef = this._def) {
    return _ZodEnum.create(values, {
      ...this._def,
      ...newDef
    });
  }
  exclude(values, newDef = this._def) {
    return _ZodEnum.create(this.options.filter((opt) => !values.includes(opt)), {
      ...this._def,
      ...newDef
    });
  }
};
ZodEnum.create = createZodEnum;
var ZodNativeEnum = class extends ZodType {
  _parse(input) {
    const nativeEnumValues = util.getValidEnumValues(this._def.values);
    const ctx = this._getOrReturnCtx(input);
    if (ctx.parsedType !== ZodParsedType.string && ctx.parsedType !== ZodParsedType.number) {
      const expectedValues = util.objectValues(nativeEnumValues);
      addIssueToContext(ctx, {
        expected: util.joinValues(expectedValues),
        received: ctx.parsedType,
        code: ZodIssueCode.invalid_type
      });
      return INVALID;
    }
    if (!this._cache) {
      this._cache = new Set(util.getValidEnumValues(this._def.values));
    }
    if (!this._cache.has(input.data)) {
      const expectedValues = util.objectValues(nativeEnumValues);
      addIssueToContext(ctx, {
        received: ctx.data,
        code: ZodIssueCode.invalid_enum_value,
        options: expectedValues
      });
      return INVALID;
    }
    return OK(input.data);
  }
  get enum() {
    return this._def.values;
  }
};
ZodNativeEnum.create = (values, params) => {
  return new ZodNativeEnum({
    values,
    typeName: ZodFirstPartyTypeKind.ZodNativeEnum,
    ...processCreateParams(params)
  });
};
var ZodPromise = class extends ZodType {
  unwrap() {
    return this._def.type;
  }
  _parse(input) {
    const { ctx } = this._processInputParams(input);
    if (ctx.parsedType !== ZodParsedType.promise && ctx.common.async === false) {
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.promise,
        received: ctx.parsedType
      });
      return INVALID;
    }
    const promisified = ctx.parsedType === ZodParsedType.promise ? ctx.data : Promise.resolve(ctx.data);
    return OK(promisified.then((data) => {
      return this._def.type.parseAsync(data, {
        path: ctx.path,
        errorMap: ctx.common.contextualErrorMap
      });
    }));
  }
};
ZodPromise.create = (schema, params) => {
  return new ZodPromise({
    type: schema,
    typeName: ZodFirstPartyTypeKind.ZodPromise,
    ...processCreateParams(params)
  });
};
var ZodEffects = class extends ZodType {
  innerType() {
    return this._def.schema;
  }
  sourceType() {
    return this._def.schema._def.typeName === ZodFirstPartyTypeKind.ZodEffects ? this._def.schema.sourceType() : this._def.schema;
  }
  _parse(input) {
    const { status, ctx } = this._processInputParams(input);
    const effect2 = this._def.effect || null;
    const checkCtx = {
      addIssue: (arg) => {
        addIssueToContext(ctx, arg);
        if (arg.fatal) {
          status.abort();
        } else {
          status.dirty();
        }
      },
      get path() {
        return ctx.path;
      }
    };
    checkCtx.addIssue = checkCtx.addIssue.bind(checkCtx);
    if (effect2.type === "preprocess") {
      const processed = effect2.transform(ctx.data, checkCtx);
      if (ctx.common.async) {
        return Promise.resolve(processed).then(async (processed2) => {
          if (status.value === "aborted")
            return INVALID;
          const result = await this._def.schema._parseAsync({
            data: processed2,
            path: ctx.path,
            parent: ctx
          });
          if (result.status === "aborted")
            return INVALID;
          if (result.status === "dirty")
            return DIRTY(result.value);
          if (status.value === "dirty")
            return DIRTY(result.value);
          return result;
        });
      } else {
        if (status.value === "aborted")
          return INVALID;
        const result = this._def.schema._parseSync({
          data: processed,
          path: ctx.path,
          parent: ctx
        });
        if (result.status === "aborted")
          return INVALID;
        if (result.status === "dirty")
          return DIRTY(result.value);
        if (status.value === "dirty")
          return DIRTY(result.value);
        return result;
      }
    }
    if (effect2.type === "refinement") {
      const executeRefinement = (acc) => {
        const result = effect2.refinement(acc, checkCtx);
        if (ctx.common.async) {
          return Promise.resolve(result);
        }
        if (result instanceof Promise) {
          throw new Error("Async refinement encountered during synchronous parse operation. Use .parseAsync instead.");
        }
        return acc;
      };
      if (ctx.common.async === false) {
        const inner = this._def.schema._parseSync({
          data: ctx.data,
          path: ctx.path,
          parent: ctx
        });
        if (inner.status === "aborted")
          return INVALID;
        if (inner.status === "dirty")
          status.dirty();
        executeRefinement(inner.value);
        return { status: status.value, value: inner.value };
      } else {
        return this._def.schema._parseAsync({ data: ctx.data, path: ctx.path, parent: ctx }).then((inner) => {
          if (inner.status === "aborted")
            return INVALID;
          if (inner.status === "dirty")
            status.dirty();
          return executeRefinement(inner.value).then(() => {
            return { status: status.value, value: inner.value };
          });
        });
      }
    }
    if (effect2.type === "transform") {
      if (ctx.common.async === false) {
        const base = this._def.schema._parseSync({
          data: ctx.data,
          path: ctx.path,
          parent: ctx
        });
        if (!isValid(base))
          return INVALID;
        const result = effect2.transform(base.value, checkCtx);
        if (result instanceof Promise) {
          throw new Error(`Asynchronous transform encountered during synchronous parse operation. Use .parseAsync instead.`);
        }
        return { status: status.value, value: result };
      } else {
        return this._def.schema._parseAsync({ data: ctx.data, path: ctx.path, parent: ctx }).then((base) => {
          if (!isValid(base))
            return INVALID;
          return Promise.resolve(effect2.transform(base.value, checkCtx)).then((result) => ({
            status: status.value,
            value: result
          }));
        });
      }
    }
    util.assertNever(effect2);
  }
};
ZodEffects.create = (schema, effect2, params) => {
  return new ZodEffects({
    schema,
    typeName: ZodFirstPartyTypeKind.ZodEffects,
    effect: effect2,
    ...processCreateParams(params)
  });
};
ZodEffects.createWithPreprocess = (preprocess, schema, params) => {
  return new ZodEffects({
    schema,
    effect: { type: "preprocess", transform: preprocess },
    typeName: ZodFirstPartyTypeKind.ZodEffects,
    ...processCreateParams(params)
  });
};
var ZodOptional = class extends ZodType {
  _parse(input) {
    const parsedType = this._getType(input);
    if (parsedType === ZodParsedType.undefined) {
      return OK(void 0);
    }
    return this._def.innerType._parse(input);
  }
  unwrap() {
    return this._def.innerType;
  }
};
ZodOptional.create = (type, params) => {
  return new ZodOptional({
    innerType: type,
    typeName: ZodFirstPartyTypeKind.ZodOptional,
    ...processCreateParams(params)
  });
};
var ZodNullable = class extends ZodType {
  _parse(input) {
    const parsedType = this._getType(input);
    if (parsedType === ZodParsedType.null) {
      return OK(null);
    }
    return this._def.innerType._parse(input);
  }
  unwrap() {
    return this._def.innerType;
  }
};
ZodNullable.create = (type, params) => {
  return new ZodNullable({
    innerType: type,
    typeName: ZodFirstPartyTypeKind.ZodNullable,
    ...processCreateParams(params)
  });
};
var ZodDefault = class extends ZodType {
  _parse(input) {
    const { ctx } = this._processInputParams(input);
    let data = ctx.data;
    if (ctx.parsedType === ZodParsedType.undefined) {
      data = this._def.defaultValue();
    }
    return this._def.innerType._parse({
      data,
      path: ctx.path,
      parent: ctx
    });
  }
  removeDefault() {
    return this._def.innerType;
  }
};
ZodDefault.create = (type, params) => {
  return new ZodDefault({
    innerType: type,
    typeName: ZodFirstPartyTypeKind.ZodDefault,
    defaultValue: typeof params.default === "function" ? params.default : () => params.default,
    ...processCreateParams(params)
  });
};
var ZodCatch = class extends ZodType {
  _parse(input) {
    const { ctx } = this._processInputParams(input);
    const newCtx = {
      ...ctx,
      common: {
        ...ctx.common,
        issues: []
      }
    };
    const result = this._def.innerType._parse({
      data: newCtx.data,
      path: newCtx.path,
      parent: {
        ...newCtx
      }
    });
    if (isAsync(result)) {
      return result.then((result2) => {
        return {
          status: "valid",
          value: result2.status === "valid" ? result2.value : this._def.catchValue({
            get error() {
              return new ZodError(newCtx.common.issues);
            },
            input: newCtx.data
          })
        };
      });
    } else {
      return {
        status: "valid",
        value: result.status === "valid" ? result.value : this._def.catchValue({
          get error() {
            return new ZodError(newCtx.common.issues);
          },
          input: newCtx.data
        })
      };
    }
  }
  removeCatch() {
    return this._def.innerType;
  }
};
ZodCatch.create = (type, params) => {
  return new ZodCatch({
    innerType: type,
    typeName: ZodFirstPartyTypeKind.ZodCatch,
    catchValue: typeof params.catch === "function" ? params.catch : () => params.catch,
    ...processCreateParams(params)
  });
};
var ZodNaN = class extends ZodType {
  _parse(input) {
    const parsedType = this._getType(input);
    if (parsedType !== ZodParsedType.nan) {
      const ctx = this._getOrReturnCtx(input);
      addIssueToContext(ctx, {
        code: ZodIssueCode.invalid_type,
        expected: ZodParsedType.nan,
        received: ctx.parsedType
      });
      return INVALID;
    }
    return { status: "valid", value: input.data };
  }
};
ZodNaN.create = (params) => {
  return new ZodNaN({
    typeName: ZodFirstPartyTypeKind.ZodNaN,
    ...processCreateParams(params)
  });
};
var BRAND = /* @__PURE__ */ Symbol("zod_brand");
var ZodBranded = class extends ZodType {
  _parse(input) {
    const { ctx } = this._processInputParams(input);
    const data = ctx.data;
    return this._def.type._parse({
      data,
      path: ctx.path,
      parent: ctx
    });
  }
  unwrap() {
    return this._def.type;
  }
};
var ZodPipeline = class _ZodPipeline extends ZodType {
  _parse(input) {
    const { status, ctx } = this._processInputParams(input);
    if (ctx.common.async) {
      const handleAsync = async () => {
        const inResult = await this._def.in._parseAsync({
          data: ctx.data,
          path: ctx.path,
          parent: ctx
        });
        if (inResult.status === "aborted")
          return INVALID;
        if (inResult.status === "dirty") {
          status.dirty();
          return DIRTY(inResult.value);
        } else {
          return this._def.out._parseAsync({
            data: inResult.value,
            path: ctx.path,
            parent: ctx
          });
        }
      };
      return handleAsync();
    } else {
      const inResult = this._def.in._parseSync({
        data: ctx.data,
        path: ctx.path,
        parent: ctx
      });
      if (inResult.status === "aborted")
        return INVALID;
      if (inResult.status === "dirty") {
        status.dirty();
        return {
          status: "dirty",
          value: inResult.value
        };
      } else {
        return this._def.out._parseSync({
          data: inResult.value,
          path: ctx.path,
          parent: ctx
        });
      }
    }
  }
  static create(a4, b4) {
    return new _ZodPipeline({
      in: a4,
      out: b4,
      typeName: ZodFirstPartyTypeKind.ZodPipeline
    });
  }
};
var ZodReadonly = class extends ZodType {
  _parse(input) {
    const result = this._def.innerType._parse(input);
    const freeze = (data) => {
      if (isValid(data)) {
        data.value = Object.freeze(data.value);
      }
      return data;
    };
    return isAsync(result) ? result.then((data) => freeze(data)) : freeze(result);
  }
  unwrap() {
    return this._def.innerType;
  }
};
ZodReadonly.create = (type, params) => {
  return new ZodReadonly({
    innerType: type,
    typeName: ZodFirstPartyTypeKind.ZodReadonly,
    ...processCreateParams(params)
  });
};
function cleanParams(params, data) {
  const p5 = typeof params === "function" ? params(data) : typeof params === "string" ? { message: params } : params;
  const p22 = typeof p5 === "string" ? { message: p5 } : p5;
  return p22;
}
function custom(check, _params = {}, fatal) {
  if (check)
    return ZodAny.create().superRefine((data, ctx) => {
      const r8 = check(data);
      if (r8 instanceof Promise) {
        return r8.then((r9) => {
          if (!r9) {
            const params = cleanParams(_params, data);
            const _fatal = params.fatal ?? fatal ?? true;
            ctx.addIssue({ code: "custom", ...params, fatal: _fatal });
          }
        });
      }
      if (!r8) {
        const params = cleanParams(_params, data);
        const _fatal = params.fatal ?? fatal ?? true;
        ctx.addIssue({ code: "custom", ...params, fatal: _fatal });
      }
      return;
    });
  return ZodAny.create();
}
var late = {
  object: ZodObject.lazycreate
};
var ZodFirstPartyTypeKind;
(function(ZodFirstPartyTypeKind2) {
  ZodFirstPartyTypeKind2["ZodString"] = "ZodString";
  ZodFirstPartyTypeKind2["ZodNumber"] = "ZodNumber";
  ZodFirstPartyTypeKind2["ZodNaN"] = "ZodNaN";
  ZodFirstPartyTypeKind2["ZodBigInt"] = "ZodBigInt";
  ZodFirstPartyTypeKind2["ZodBoolean"] = "ZodBoolean";
  ZodFirstPartyTypeKind2["ZodDate"] = "ZodDate";
  ZodFirstPartyTypeKind2["ZodSymbol"] = "ZodSymbol";
  ZodFirstPartyTypeKind2["ZodUndefined"] = "ZodUndefined";
  ZodFirstPartyTypeKind2["ZodNull"] = "ZodNull";
  ZodFirstPartyTypeKind2["ZodAny"] = "ZodAny";
  ZodFirstPartyTypeKind2["ZodUnknown"] = "ZodUnknown";
  ZodFirstPartyTypeKind2["ZodNever"] = "ZodNever";
  ZodFirstPartyTypeKind2["ZodVoid"] = "ZodVoid";
  ZodFirstPartyTypeKind2["ZodArray"] = "ZodArray";
  ZodFirstPartyTypeKind2["ZodObject"] = "ZodObject";
  ZodFirstPartyTypeKind2["ZodUnion"] = "ZodUnion";
  ZodFirstPartyTypeKind2["ZodDiscriminatedUnion"] = "ZodDiscriminatedUnion";
  ZodFirstPartyTypeKind2["ZodIntersection"] = "ZodIntersection";
  ZodFirstPartyTypeKind2["ZodTuple"] = "ZodTuple";
  ZodFirstPartyTypeKind2["ZodRecord"] = "ZodRecord";
  ZodFirstPartyTypeKind2["ZodMap"] = "ZodMap";
  ZodFirstPartyTypeKind2["ZodSet"] = "ZodSet";
  ZodFirstPartyTypeKind2["ZodFunction"] = "ZodFunction";
  ZodFirstPartyTypeKind2["ZodLazy"] = "ZodLazy";
  ZodFirstPartyTypeKind2["ZodLiteral"] = "ZodLiteral";
  ZodFirstPartyTypeKind2["ZodEnum"] = "ZodEnum";
  ZodFirstPartyTypeKind2["ZodEffects"] = "ZodEffects";
  ZodFirstPartyTypeKind2["ZodNativeEnum"] = "ZodNativeEnum";
  ZodFirstPartyTypeKind2["ZodOptional"] = "ZodOptional";
  ZodFirstPartyTypeKind2["ZodNullable"] = "ZodNullable";
  ZodFirstPartyTypeKind2["ZodDefault"] = "ZodDefault";
  ZodFirstPartyTypeKind2["ZodCatch"] = "ZodCatch";
  ZodFirstPartyTypeKind2["ZodPromise"] = "ZodPromise";
  ZodFirstPartyTypeKind2["ZodBranded"] = "ZodBranded";
  ZodFirstPartyTypeKind2["ZodPipeline"] = "ZodPipeline";
  ZodFirstPartyTypeKind2["ZodReadonly"] = "ZodReadonly";
})(ZodFirstPartyTypeKind || (ZodFirstPartyTypeKind = {}));
var instanceOfType = (cls, params = {
  message: `Input not instance of ${cls.name}`
}) => custom((data) => data instanceof cls, params);
var stringType = ZodString.create;
var numberType = ZodNumber.create;
var nanType = ZodNaN.create;
var bigIntType = ZodBigInt.create;
var booleanType = ZodBoolean.create;
var dateType = ZodDate.create;
var symbolType = ZodSymbol.create;
var undefinedType = ZodUndefined.create;
var nullType = ZodNull.create;
var anyType = ZodAny.create;
var unknownType = ZodUnknown.create;
var neverType = ZodNever.create;
var voidType = ZodVoid.create;
var arrayType = ZodArray.create;
var objectType = ZodObject.create;
var strictObjectType = ZodObject.strictCreate;
var unionType = ZodUnion.create;
var discriminatedUnionType = ZodDiscriminatedUnion.create;
var intersectionType = ZodIntersection.create;
var tupleType = ZodTuple.create;
var recordType = ZodRecord.create;
var mapType = ZodMap.create;
var setType = ZodSet.create;
var functionType = ZodFunction.create;
var lazyType = ZodLazy.create;
var literalType = ZodLiteral.create;
var enumType = ZodEnum.create;
var nativeEnumType = ZodNativeEnum.create;
var promiseType = ZodPromise.create;
var effectsType = ZodEffects.create;
var optionalType = ZodOptional.create;
var nullableType = ZodNullable.create;
var preprocessType = ZodEffects.createWithPreprocess;
var pipelineType = ZodPipeline.create;
var ostring = () => stringType().optional();
var onumber = () => numberType().optional();
var oboolean = () => booleanType().optional();
var coerce = {
  string: ((arg) => ZodString.create({ ...arg, coerce: true })),
  number: ((arg) => ZodNumber.create({ ...arg, coerce: true })),
  boolean: ((arg) => ZodBoolean.create({
    ...arg,
    coerce: true
  })),
  bigint: ((arg) => ZodBigInt.create({ ...arg, coerce: true })),
  date: ((arg) => ZodDate.create({ ...arg, coerce: true }))
};
var NEVER = INVALID;

// node_modules/zod-to-json-schema/dist/esm/parsers/any.js
function parseAnyDef(refs) {
  if (refs.target !== "openAi") {
    return {};
  }
  const anyDefinitionPath = [
    ...refs.basePath,
    refs.definitionPath,
    refs.openAiAnyTypeName
  ];
  refs.flags.hasReferencedOpenAiAnyType = true;
  return {
    $ref: refs.$refStrategy === "relative" ? getRelativePath(anyDefinitionPath, refs.currentPath) : anyDefinitionPath.join("/")
  };
}

// node_modules/zod-to-json-schema/dist/esm/parsers/array.js
function parseArrayDef(def, refs) {
  const res = {
    type: "array"
  };
  if (def.type?._def && def.type?._def?.typeName !== ZodFirstPartyTypeKind.ZodAny) {
    res.items = parseDef(def.type._def, {
      ...refs,
      currentPath: [...refs.currentPath, "items"]
    });
  }
  if (def.minLength) {
    setResponseValueAndErrors(res, "minItems", def.minLength.value, def.minLength.message, refs);
  }
  if (def.maxLength) {
    setResponseValueAndErrors(res, "maxItems", def.maxLength.value, def.maxLength.message, refs);
  }
  if (def.exactLength) {
    setResponseValueAndErrors(res, "minItems", def.exactLength.value, def.exactLength.message, refs);
    setResponseValueAndErrors(res, "maxItems", def.exactLength.value, def.exactLength.message, refs);
  }
  return res;
}

// node_modules/zod-to-json-schema/dist/esm/parsers/bigint.js
function parseBigintDef(def, refs) {
  const res = {
    type: "integer",
    format: "int64"
  };
  if (!def.checks)
    return res;
  for (const check of def.checks) {
    switch (check.kind) {
      case "min":
        if (refs.target === "jsonSchema7") {
          if (check.inclusive) {
            setResponseValueAndErrors(res, "minimum", check.value, check.message, refs);
          } else {
            setResponseValueAndErrors(res, "exclusiveMinimum", check.value, check.message, refs);
          }
        } else {
          if (!check.inclusive) {
            res.exclusiveMinimum = true;
          }
          setResponseValueAndErrors(res, "minimum", check.value, check.message, refs);
        }
        break;
      case "max":
        if (refs.target === "jsonSchema7") {
          if (check.inclusive) {
            setResponseValueAndErrors(res, "maximum", check.value, check.message, refs);
          } else {
            setResponseValueAndErrors(res, "exclusiveMaximum", check.value, check.message, refs);
          }
        } else {
          if (!check.inclusive) {
            res.exclusiveMaximum = true;
          }
          setResponseValueAndErrors(res, "maximum", check.value, check.message, refs);
        }
        break;
      case "multipleOf":
        setResponseValueAndErrors(res, "multipleOf", check.value, check.message, refs);
        break;
    }
  }
  return res;
}

// node_modules/zod-to-json-schema/dist/esm/parsers/boolean.js
function parseBooleanDef() {
  return {
    type: "boolean"
  };
}

// node_modules/zod-to-json-schema/dist/esm/parsers/branded.js
function parseBrandedDef(_def, refs) {
  return parseDef(_def.type._def, refs);
}

// node_modules/zod-to-json-schema/dist/esm/parsers/catch.js
var parseCatchDef = (def, refs) => {
  return parseDef(def.innerType._def, refs);
};

// node_modules/zod-to-json-schema/dist/esm/parsers/date.js
function parseDateDef(def, refs, overrideDateStrategy) {
  const strategy = overrideDateStrategy ?? refs.dateStrategy;
  if (Array.isArray(strategy)) {
    return {
      anyOf: strategy.map((item, i7) => parseDateDef(def, refs, item))
    };
  }
  switch (strategy) {
    case "string":
    case "format:date-time":
      return {
        type: "string",
        format: "date-time"
      };
    case "format:date":
      return {
        type: "string",
        format: "date"
      };
    case "integer":
      return integerDateParser(def, refs);
  }
}
var integerDateParser = (def, refs) => {
  const res = {
    type: "integer",
    format: "unix-time"
  };
  if (refs.target === "openApi3") {
    return res;
  }
  for (const check of def.checks) {
    switch (check.kind) {
      case "min":
        setResponseValueAndErrors(
          res,
          "minimum",
          check.value,
          // This is in milliseconds
          check.message,
          refs
        );
        break;
      case "max":
        setResponseValueAndErrors(
          res,
          "maximum",
          check.value,
          // This is in milliseconds
          check.message,
          refs
        );
        break;
    }
  }
  return res;
};

// node_modules/zod-to-json-schema/dist/esm/parsers/default.js
function parseDefaultDef(_def, refs) {
  return {
    ...parseDef(_def.innerType._def, refs),
    default: _def.defaultValue()
  };
}

// node_modules/zod-to-json-schema/dist/esm/parsers/effects.js
function parseEffectsDef(_def, refs) {
  return refs.effectStrategy === "input" ? parseDef(_def.schema._def, refs) : parseAnyDef(refs);
}

// node_modules/zod-to-json-schema/dist/esm/parsers/enum.js
function parseEnumDef(def) {
  return {
    type: "string",
    enum: Array.from(def.values)
  };
}

// node_modules/zod-to-json-schema/dist/esm/parsers/intersection.js
var isJsonSchema7AllOfType = (type) => {
  if ("type" in type && type.type === "string")
    return false;
  return "allOf" in type;
};
function parseIntersectionDef(def, refs) {
  const allOf = [
    parseDef(def.left._def, {
      ...refs,
      currentPath: [...refs.currentPath, "allOf", "0"]
    }),
    parseDef(def.right._def, {
      ...refs,
      currentPath: [...refs.currentPath, "allOf", "1"]
    })
  ].filter((x3) => !!x3);
  let unevaluatedProperties = refs.target === "jsonSchema2019-09" ? { unevaluatedProperties: false } : void 0;
  const mergedAllOf = [];
  allOf.forEach((schema) => {
    if (isJsonSchema7AllOfType(schema)) {
      mergedAllOf.push(...schema.allOf);
      if (schema.unevaluatedProperties === void 0) {
        unevaluatedProperties = void 0;
      }
    } else {
      let nestedSchema = schema;
      if ("additionalProperties" in schema && schema.additionalProperties === false) {
        const { additionalProperties, ...rest } = schema;
        nestedSchema = rest;
      } else {
        unevaluatedProperties = void 0;
      }
      mergedAllOf.push(nestedSchema);
    }
  });
  return mergedAllOf.length ? {
    allOf: mergedAllOf,
    ...unevaluatedProperties
  } : void 0;
}

// node_modules/zod-to-json-schema/dist/esm/parsers/literal.js
function parseLiteralDef(def, refs) {
  const parsedType = typeof def.value;
  if (parsedType !== "bigint" && parsedType !== "number" && parsedType !== "boolean" && parsedType !== "string") {
    return {
      type: Array.isArray(def.value) ? "array" : "object"
    };
  }
  if (refs.target === "openApi3") {
    return {
      type: parsedType === "bigint" ? "integer" : parsedType,
      enum: [def.value]
    };
  }
  return {
    type: parsedType === "bigint" ? "integer" : parsedType,
    const: def.value
  };
}

// node_modules/zod-to-json-schema/dist/esm/parsers/string.js
var emojiRegex2 = void 0;
var zodPatterns = {
  /**
   * `c` was changed to `[cC]` to replicate /i flag
   */
  cuid: /^[cC][^\s-]{8,}$/,
  cuid2: /^[0-9a-z]+$/,
  ulid: /^[0-9A-HJKMNP-TV-Z]{26}$/,
  /**
   * `a-z` was added to replicate /i flag
   */
  email: /^(?!\.)(?!.*\.\.)([a-zA-Z0-9_'+\-\.]*)[a-zA-Z0-9_+-]@([a-zA-Z0-9][a-zA-Z0-9\-]*\.)+[a-zA-Z]{2,}$/,
  /**
   * Constructed a valid Unicode RegExp
   *
   * Lazily instantiate since this type of regex isn't supported
   * in all envs (e.g. React Native).
   *
   * See:
   * https://github.com/colinhacks/zod/issues/2433
   * Fix in Zod:
   * https://github.com/colinhacks/zod/commit/9340fd51e48576a75adc919bff65dbc4a5d4c99b
   */
  emoji: () => {
    if (emojiRegex2 === void 0) {
      emojiRegex2 = RegExp("^(\\p{Extended_Pictographic}|\\p{Emoji_Component})+$", "u");
    }
    return emojiRegex2;
  },
  /**
   * Unused
   */
  uuid: /^[0-9a-fA-F]{8}\b-[0-9a-fA-F]{4}\b-[0-9a-fA-F]{4}\b-[0-9a-fA-F]{4}\b-[0-9a-fA-F]{12}$/,
  /**
   * Unused
   */
  ipv4: /^(?:(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])$/,
  ipv4Cidr: /^(?:(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9][0-9]|[0-9])\/(3[0-2]|[12]?[0-9])$/,
  /**
   * Unused
   */
  ipv6: /^(([a-f0-9]{1,4}:){7}|::([a-f0-9]{1,4}:){0,6}|([a-f0-9]{1,4}:){1}:([a-f0-9]{1,4}:){0,5}|([a-f0-9]{1,4}:){2}:([a-f0-9]{1,4}:){0,4}|([a-f0-9]{1,4}:){3}:([a-f0-9]{1,4}:){0,3}|([a-f0-9]{1,4}:){4}:([a-f0-9]{1,4}:){0,2}|([a-f0-9]{1,4}:){5}:([a-f0-9]{1,4}:){0,1})([a-f0-9]{1,4}|(((25[0-5])|(2[0-4][0-9])|(1[0-9]{2})|([0-9]{1,2}))\.){3}((25[0-5])|(2[0-4][0-9])|(1[0-9]{2})|([0-9]{1,2})))$/,
  ipv6Cidr: /^(([0-9a-fA-F]{1,4}:){7,7}[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,7}:|([0-9a-fA-F]{1,4}:){1,6}:[0-9a-fA-F]{1,4}|([0-9a-fA-F]{1,4}:){1,5}(:[0-9a-fA-F]{1,4}){1,2}|([0-9a-fA-F]{1,4}:){1,4}(:[0-9a-fA-F]{1,4}){1,3}|([0-9a-fA-F]{1,4}:){1,3}(:[0-9a-fA-F]{1,4}){1,4}|([0-9a-fA-F]{1,4}:){1,2}(:[0-9a-fA-F]{1,4}){1,5}|[0-9a-fA-F]{1,4}:((:[0-9a-fA-F]{1,4}){1,6})|:((:[0-9a-fA-F]{1,4}){1,7}|:)|fe80:(:[0-9a-fA-F]{0,4}){0,4}%[0-9a-zA-Z]{1,}|::(ffff(:0{1,4}){0,1}:){0,1}((25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])|([0-9a-fA-F]{1,4}:){1,4}:((25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9])\.){3,3}(25[0-5]|(2[0-4]|1{0,1}[0-9]){0,1}[0-9]))\/(12[0-8]|1[01][0-9]|[1-9]?[0-9])$/,
  base64: /^([0-9a-zA-Z+/]{4})*(([0-9a-zA-Z+/]{2}==)|([0-9a-zA-Z+/]{3}=))?$/,
  base64url: /^([0-9a-zA-Z-_]{4})*(([0-9a-zA-Z-_]{2}(==)?)|([0-9a-zA-Z-_]{3}(=)?))?$/,
  nanoid: /^[a-zA-Z0-9_-]{21}$/,
  jwt: /^[A-Za-z0-9-_]+\.[A-Za-z0-9-_]+\.[A-Za-z0-9-_]*$/
};
function parseStringDef(def, refs) {
  const res = {
    type: "string"
  };
  if (def.checks) {
    for (const check of def.checks) {
      switch (check.kind) {
        case "min":
          setResponseValueAndErrors(res, "minLength", typeof res.minLength === "number" ? Math.max(res.minLength, check.value) : check.value, check.message, refs);
          break;
        case "max":
          setResponseValueAndErrors(res, "maxLength", typeof res.maxLength === "number" ? Math.min(res.maxLength, check.value) : check.value, check.message, refs);
          break;
        case "email":
          switch (refs.emailStrategy) {
            case "format:email":
              addFormat(res, "email", check.message, refs);
              break;
            case "format:idn-email":
              addFormat(res, "idn-email", check.message, refs);
              break;
            case "pattern:zod":
              addPattern(res, zodPatterns.email, check.message, refs);
              break;
          }
          break;
        case "url":
          addFormat(res, "uri", check.message, refs);
          break;
        case "uuid":
          addFormat(res, "uuid", check.message, refs);
          break;
        case "regex":
          addPattern(res, check.regex, check.message, refs);
          break;
        case "cuid":
          addPattern(res, zodPatterns.cuid, check.message, refs);
          break;
        case "cuid2":
          addPattern(res, zodPatterns.cuid2, check.message, refs);
          break;
        case "startsWith":
          addPattern(res, RegExp(`^${escapeLiteralCheckValue(check.value, refs)}`), check.message, refs);
          break;
        case "endsWith":
          addPattern(res, RegExp(`${escapeLiteralCheckValue(check.value, refs)}$`), check.message, refs);
          break;
        case "datetime":
          addFormat(res, "date-time", check.message, refs);
          break;
        case "date":
          addFormat(res, "date", check.message, refs);
          break;
        case "time":
          addFormat(res, "time", check.message, refs);
          break;
        case "duration":
          addFormat(res, "duration", check.message, refs);
          break;
        case "length":
          setResponseValueAndErrors(res, "minLength", typeof res.minLength === "number" ? Math.max(res.minLength, check.value) : check.value, check.message, refs);
          setResponseValueAndErrors(res, "maxLength", typeof res.maxLength === "number" ? Math.min(res.maxLength, check.value) : check.value, check.message, refs);
          break;
        case "includes": {
          addPattern(res, RegExp(escapeLiteralCheckValue(check.value, refs)), check.message, refs);
          break;
        }
        case "ip": {
          if (check.version !== "v6") {
            addFormat(res, "ipv4", check.message, refs);
          }
          if (check.version !== "v4") {
            addFormat(res, "ipv6", check.message, refs);
          }
          break;
        }
        case "base64url":
          addPattern(res, zodPatterns.base64url, check.message, refs);
          break;
        case "jwt":
          addPattern(res, zodPatterns.jwt, check.message, refs);
          break;
        case "cidr": {
          if (check.version !== "v6") {
            addPattern(res, zodPatterns.ipv4Cidr, check.message, refs);
          }
          if (check.version !== "v4") {
            addPattern(res, zodPatterns.ipv6Cidr, check.message, refs);
          }
          break;
        }
        case "emoji":
          addPattern(res, zodPatterns.emoji(), check.message, refs);
          break;
        case "ulid": {
          addPattern(res, zodPatterns.ulid, check.message, refs);
          break;
        }
        case "base64": {
          switch (refs.base64Strategy) {
            case "format:binary": {
              addFormat(res, "binary", check.message, refs);
              break;
            }
            case "contentEncoding:base64": {
              setResponseValueAndErrors(res, "contentEncoding", "base64", check.message, refs);
              break;
            }
            case "pattern:zod": {
              addPattern(res, zodPatterns.base64, check.message, refs);
              break;
            }
          }
          break;
        }
        case "nanoid": {
          addPattern(res, zodPatterns.nanoid, check.message, refs);
        }
        case "toLowerCase":
        case "toUpperCase":
        case "trim":
          break;
        default:
          /* @__PURE__ */ ((_3) => {
          })(check);
      }
    }
  }
  return res;
}
function escapeLiteralCheckValue(literal, refs) {
  return refs.patternStrategy === "escape" ? escapeNonAlphaNumeric(literal) : literal;
}
var ALPHA_NUMERIC = new Set("ABCDEFGHIJKLMNOPQRSTUVXYZabcdefghijklmnopqrstuvxyz0123456789");
function escapeNonAlphaNumeric(source) {
  let result = "";
  for (let i7 = 0; i7 < source.length; i7++) {
    if (!ALPHA_NUMERIC.has(source[i7])) {
      result += "\\";
    }
    result += source[i7];
  }
  return result;
}
function addFormat(schema, value, message, refs) {
  if (schema.format || schema.anyOf?.some((x3) => x3.format)) {
    if (!schema.anyOf) {
      schema.anyOf = [];
    }
    if (schema.format) {
      schema.anyOf.push({
        format: schema.format,
        ...schema.errorMessage && refs.errorMessages && {
          errorMessage: { format: schema.errorMessage.format }
        }
      });
      delete schema.format;
      if (schema.errorMessage) {
        delete schema.errorMessage.format;
        if (Object.keys(schema.errorMessage).length === 0) {
          delete schema.errorMessage;
        }
      }
    }
    schema.anyOf.push({
      format: value,
      ...message && refs.errorMessages && { errorMessage: { format: message } }
    });
  } else {
    setResponseValueAndErrors(schema, "format", value, message, refs);
  }
}
function addPattern(schema, regex, message, refs) {
  if (schema.pattern || schema.allOf?.some((x3) => x3.pattern)) {
    if (!schema.allOf) {
      schema.allOf = [];
    }
    if (schema.pattern) {
      schema.allOf.push({
        pattern: schema.pattern,
        ...schema.errorMessage && refs.errorMessages && {
          errorMessage: { pattern: schema.errorMessage.pattern }
        }
      });
      delete schema.pattern;
      if (schema.errorMessage) {
        delete schema.errorMessage.pattern;
        if (Object.keys(schema.errorMessage).length === 0) {
          delete schema.errorMessage;
        }
      }
    }
    schema.allOf.push({
      pattern: stringifyRegExpWithFlags(regex, refs),
      ...message && refs.errorMessages && { errorMessage: { pattern: message } }
    });
  } else {
    setResponseValueAndErrors(schema, "pattern", stringifyRegExpWithFlags(regex, refs), message, refs);
  }
}
function stringifyRegExpWithFlags(regex, refs) {
  if (!refs.applyRegexFlags || !regex.flags) {
    return regex.source;
  }
  const flags = {
    i: regex.flags.includes("i"),
    m: regex.flags.includes("m"),
    s: regex.flags.includes("s")
    // `.` matches newlines
  };
  const source = flags.i ? regex.source.toLowerCase() : regex.source;
  let pattern = "";
  let isEscaped = false;
  let inCharGroup = false;
  let inCharRange = false;
  for (let i7 = 0; i7 < source.length; i7++) {
    if (isEscaped) {
      pattern += source[i7];
      isEscaped = false;
      continue;
    }
    if (flags.i) {
      if (inCharGroup) {
        if (source[i7].match(/[a-z]/)) {
          if (inCharRange) {
            pattern += source[i7];
            pattern += `${source[i7 - 2]}-${source[i7]}`.toUpperCase();
            inCharRange = false;
          } else if (source[i7 + 1] === "-" && source[i7 + 2]?.match(/[a-z]/)) {
            pattern += source[i7];
            inCharRange = true;
          } else {
            pattern += `${source[i7]}${source[i7].toUpperCase()}`;
          }
          continue;
        }
      } else if (source[i7].match(/[a-z]/)) {
        pattern += `[${source[i7]}${source[i7].toUpperCase()}]`;
        continue;
      }
    }
    if (flags.m) {
      if (source[i7] === "^") {
        pattern += `(^|(?<=[\r
]))`;
        continue;
      } else if (source[i7] === "$") {
        pattern += `($|(?=[\r
]))`;
        continue;
      }
    }
    if (flags.s && source[i7] === ".") {
      pattern += inCharGroup ? `${source[i7]}\r
` : `[${source[i7]}\r
]`;
      continue;
    }
    pattern += source[i7];
    if (source[i7] === "\\") {
      isEscaped = true;
    } else if (inCharGroup && source[i7] === "]") {
      inCharGroup = false;
    } else if (!inCharGroup && source[i7] === "[") {
      inCharGroup = true;
    }
  }
  try {
    new RegExp(pattern);
  } catch {
    console.warn(`Could not convert regex pattern at ${refs.currentPath.join("/")} to a flag-independent form! Falling back to the flag-ignorant source`);
    return regex.source;
  }
  return pattern;
}

// node_modules/zod-to-json-schema/dist/esm/parsers/record.js
function parseRecordDef(def, refs) {
  if (refs.target === "openAi") {
    console.warn("Warning: OpenAI may not support records in schemas! Try an array of key-value pairs instead.");
  }
  if (refs.target === "openApi3" && def.keyType?._def.typeName === ZodFirstPartyTypeKind.ZodEnum) {
    return {
      type: "object",
      required: def.keyType._def.values,
      properties: def.keyType._def.values.reduce((acc, key) => ({
        ...acc,
        [key]: parseDef(def.valueType._def, {
          ...refs,
          currentPath: [...refs.currentPath, "properties", key]
        }) ?? parseAnyDef(refs)
      }), {}),
      additionalProperties: refs.rejectedAdditionalProperties
    };
  }
  const schema = {
    type: "object",
    additionalProperties: parseDef(def.valueType._def, {
      ...refs,
      currentPath: [...refs.currentPath, "additionalProperties"]
    }) ?? refs.allowedAdditionalProperties
  };
  if (refs.target === "openApi3") {
    return schema;
  }
  if (def.keyType?._def.typeName === ZodFirstPartyTypeKind.ZodString && def.keyType._def.checks?.length) {
    const { type, ...keyType } = parseStringDef(def.keyType._def, refs);
    return {
      ...schema,
      propertyNames: keyType
    };
  } else if (def.keyType?._def.typeName === ZodFirstPartyTypeKind.ZodEnum) {
    return {
      ...schema,
      propertyNames: {
        enum: def.keyType._def.values
      }
    };
  } else if (def.keyType?._def.typeName === ZodFirstPartyTypeKind.ZodBranded && def.keyType._def.type._def.typeName === ZodFirstPartyTypeKind.ZodString && def.keyType._def.type._def.checks?.length) {
    const { type, ...keyType } = parseBrandedDef(def.keyType._def, refs);
    return {
      ...schema,
      propertyNames: keyType
    };
  }
  return schema;
}

// node_modules/zod-to-json-schema/dist/esm/parsers/map.js
function parseMapDef(def, refs) {
  if (refs.mapStrategy === "record") {
    return parseRecordDef(def, refs);
  }
  const keys = parseDef(def.keyType._def, {
    ...refs,
    currentPath: [...refs.currentPath, "items", "items", "0"]
  }) || parseAnyDef(refs);
  const values = parseDef(def.valueType._def, {
    ...refs,
    currentPath: [...refs.currentPath, "items", "items", "1"]
  }) || parseAnyDef(refs);
  return {
    type: "array",
    maxItems: 125,
    items: {
      type: "array",
      items: [keys, values],
      minItems: 2,
      maxItems: 2
    }
  };
}

// node_modules/zod-to-json-schema/dist/esm/parsers/nativeEnum.js
function parseNativeEnumDef(def) {
  const object = def.values;
  const actualKeys = Object.keys(def.values).filter((key) => {
    return typeof object[object[key]] !== "number";
  });
  const actualValues = actualKeys.map((key) => object[key]);
  const parsedTypes = Array.from(new Set(actualValues.map((values) => typeof values)));
  return {
    type: parsedTypes.length === 1 ? parsedTypes[0] === "string" ? "string" : "number" : ["string", "number"],
    enum: actualValues
  };
}

// node_modules/zod-to-json-schema/dist/esm/parsers/never.js
function parseNeverDef(refs) {
  return refs.target === "openAi" ? void 0 : {
    not: parseAnyDef({
      ...refs,
      currentPath: [...refs.currentPath, "not"]
    })
  };
}

// node_modules/zod-to-json-schema/dist/esm/parsers/null.js
function parseNullDef(refs) {
  return refs.target === "openApi3" ? {
    enum: ["null"],
    nullable: true
  } : {
    type: "null"
  };
}

// node_modules/zod-to-json-schema/dist/esm/parsers/union.js
var primitiveMappings = {
  ZodString: "string",
  ZodNumber: "number",
  ZodBigInt: "integer",
  ZodBoolean: "boolean",
  ZodNull: "null"
};
function parseUnionDef(def, refs) {
  if (refs.target === "openApi3")
    return asAnyOf(def, refs);
  const options = def.options instanceof Map ? Array.from(def.options.values()) : def.options;
  if (options.every((x3) => x3._def.typeName in primitiveMappings && (!x3._def.checks || !x3._def.checks.length))) {
    const types = options.reduce((types2, x3) => {
      const type = primitiveMappings[x3._def.typeName];
      return type && !types2.includes(type) ? [...types2, type] : types2;
    }, []);
    return {
      type: types.length > 1 ? types : types[0]
    };
  } else if (options.every((x3) => x3._def.typeName === "ZodLiteral" && !x3.description)) {
    const types = options.reduce((acc, x3) => {
      const type = typeof x3._def.value;
      switch (type) {
        case "string":
        case "number":
        case "boolean":
          return [...acc, type];
        case "bigint":
          return [...acc, "integer"];
        case "object":
          if (x3._def.value === null)
            return [...acc, "null"];
        case "symbol":
        case "undefined":
        case "function":
        default:
          return acc;
      }
    }, []);
    if (types.length === options.length) {
      const uniqueTypes = types.filter((x3, i7, a4) => a4.indexOf(x3) === i7);
      return {
        type: uniqueTypes.length > 1 ? uniqueTypes : uniqueTypes[0],
        enum: options.reduce((acc, x3) => {
          return acc.includes(x3._def.value) ? acc : [...acc, x3._def.value];
        }, [])
      };
    }
  } else if (options.every((x3) => x3._def.typeName === "ZodEnum")) {
    return {
      type: "string",
      enum: options.reduce((acc, x3) => [
        ...acc,
        ...x3._def.values.filter((x4) => !acc.includes(x4))
      ], [])
    };
  }
  return asAnyOf(def, refs);
}
var asAnyOf = (def, refs) => {
  const anyOf = (def.options instanceof Map ? Array.from(def.options.values()) : def.options).map((x3, i7) => parseDef(x3._def, {
    ...refs,
    currentPath: [...refs.currentPath, "anyOf", `${i7}`]
  })).filter((x3) => !!x3 && (!refs.strictUnions || typeof x3 === "object" && Object.keys(x3).length > 0));
  return anyOf.length ? { anyOf } : void 0;
};

// node_modules/zod-to-json-schema/dist/esm/parsers/nullable.js
function parseNullableDef(def, refs) {
  if (["ZodString", "ZodNumber", "ZodBigInt", "ZodBoolean", "ZodNull"].includes(def.innerType._def.typeName) && (!def.innerType._def.checks || !def.innerType._def.checks.length)) {
    if (refs.target === "openApi3") {
      return {
        type: primitiveMappings[def.innerType._def.typeName],
        nullable: true
      };
    }
    return {
      type: [
        primitiveMappings[def.innerType._def.typeName],
        "null"
      ]
    };
  }
  if (refs.target === "openApi3") {
    const base2 = parseDef(def.innerType._def, {
      ...refs,
      currentPath: [...refs.currentPath]
    });
    if (base2 && "$ref" in base2)
      return { allOf: [base2], nullable: true };
    return base2 && { ...base2, nullable: true };
  }
  const base = parseDef(def.innerType._def, {
    ...refs,
    currentPath: [...refs.currentPath, "anyOf", "0"]
  });
  return base && { anyOf: [base, { type: "null" }] };
}

// node_modules/zod-to-json-schema/dist/esm/parsers/number.js
function parseNumberDef(def, refs) {
  const res = {
    type: "number"
  };
  if (!def.checks)
    return res;
  for (const check of def.checks) {
    switch (check.kind) {
      case "int":
        res.type = "integer";
        addErrorMessage(res, "type", check.message, refs);
        break;
      case "min":
        if (refs.target === "jsonSchema7") {
          if (check.inclusive) {
            setResponseValueAndErrors(res, "minimum", check.value, check.message, refs);
          } else {
            setResponseValueAndErrors(res, "exclusiveMinimum", check.value, check.message, refs);
          }
        } else {
          if (!check.inclusive) {
            res.exclusiveMinimum = true;
          }
          setResponseValueAndErrors(res, "minimum", check.value, check.message, refs);
        }
        break;
      case "max":
        if (refs.target === "jsonSchema7") {
          if (check.inclusive) {
            setResponseValueAndErrors(res, "maximum", check.value, check.message, refs);
          } else {
            setResponseValueAndErrors(res, "exclusiveMaximum", check.value, check.message, refs);
          }
        } else {
          if (!check.inclusive) {
            res.exclusiveMaximum = true;
          }
          setResponseValueAndErrors(res, "maximum", check.value, check.message, refs);
        }
        break;
      case "multipleOf":
        setResponseValueAndErrors(res, "multipleOf", check.value, check.message, refs);
        break;
    }
  }
  return res;
}

// node_modules/zod-to-json-schema/dist/esm/parsers/object.js
function parseObjectDef(def, refs) {
  const forceOptionalIntoNullable = refs.target === "openAi";
  const result = {
    type: "object",
    properties: {}
  };
  const required = [];
  const shape = def.shape();
  for (const propName in shape) {
    let propDef = shape[propName];
    if (propDef === void 0 || propDef._def === void 0) {
      continue;
    }
    let propOptional = safeIsOptional(propDef);
    if (propOptional && forceOptionalIntoNullable) {
      if (propDef._def.typeName === "ZodOptional") {
        propDef = propDef._def.innerType;
      }
      if (!propDef.isNullable()) {
        propDef = propDef.nullable();
      }
      propOptional = false;
    }
    const parsedDef = parseDef(propDef._def, {
      ...refs,
      currentPath: [...refs.currentPath, "properties", propName],
      propertyPath: [...refs.currentPath, "properties", propName]
    });
    if (parsedDef === void 0) {
      continue;
    }
    result.properties[propName] = parsedDef;
    if (!propOptional) {
      required.push(propName);
    }
  }
  if (required.length) {
    result.required = required;
  }
  const additionalProperties = decideAdditionalProperties(def, refs);
  if (additionalProperties !== void 0) {
    result.additionalProperties = additionalProperties;
  }
  return result;
}
function decideAdditionalProperties(def, refs) {
  if (def.catchall._def.typeName !== "ZodNever") {
    return parseDef(def.catchall._def, {
      ...refs,
      currentPath: [...refs.currentPath, "additionalProperties"]
    });
  }
  switch (def.unknownKeys) {
    case "passthrough":
      return refs.allowedAdditionalProperties;
    case "strict":
      return refs.rejectedAdditionalProperties;
    case "strip":
      return refs.removeAdditionalStrategy === "strict" ? refs.allowedAdditionalProperties : refs.rejectedAdditionalProperties;
  }
}
function safeIsOptional(schema) {
  try {
    return schema.isOptional();
  } catch {
    return true;
  }
}

// node_modules/zod-to-json-schema/dist/esm/parsers/optional.js
var parseOptionalDef = (def, refs) => {
  if (refs.currentPath.toString() === refs.propertyPath?.toString()) {
    return parseDef(def.innerType._def, refs);
  }
  const innerSchema3 = parseDef(def.innerType._def, {
    ...refs,
    currentPath: [...refs.currentPath, "anyOf", "1"]
  });
  return innerSchema3 ? {
    anyOf: [
      {
        not: parseAnyDef(refs)
      },
      innerSchema3
    ]
  } : parseAnyDef(refs);
};

// node_modules/zod-to-json-schema/dist/esm/parsers/pipeline.js
var parsePipelineDef = (def, refs) => {
  if (refs.pipeStrategy === "input") {
    return parseDef(def.in._def, refs);
  } else if (refs.pipeStrategy === "output") {
    return parseDef(def.out._def, refs);
  }
  const a4 = parseDef(def.in._def, {
    ...refs,
    currentPath: [...refs.currentPath, "allOf", "0"]
  });
  const b4 = parseDef(def.out._def, {
    ...refs,
    currentPath: [...refs.currentPath, "allOf", a4 ? "1" : "0"]
  });
  return {
    allOf: [a4, b4].filter((x3) => x3 !== void 0)
  };
};

// node_modules/zod-to-json-schema/dist/esm/parsers/promise.js
function parsePromiseDef(def, refs) {
  return parseDef(def.type._def, refs);
}

// node_modules/zod-to-json-schema/dist/esm/parsers/set.js
function parseSetDef(def, refs) {
  const items = parseDef(def.valueType._def, {
    ...refs,
    currentPath: [...refs.currentPath, "items"]
  });
  const schema = {
    type: "array",
    uniqueItems: true,
    items
  };
  if (def.minSize) {
    setResponseValueAndErrors(schema, "minItems", def.minSize.value, def.minSize.message, refs);
  }
  if (def.maxSize) {
    setResponseValueAndErrors(schema, "maxItems", def.maxSize.value, def.maxSize.message, refs);
  }
  return schema;
}

// node_modules/zod-to-json-schema/dist/esm/parsers/tuple.js
function parseTupleDef(def, refs) {
  if (def.rest) {
    return {
      type: "array",
      minItems: def.items.length,
      items: def.items.map((x3, i7) => parseDef(x3._def, {
        ...refs,
        currentPath: [...refs.currentPath, "items", `${i7}`]
      })).reduce((acc, x3) => x3 === void 0 ? acc : [...acc, x3], []),
      additionalItems: parseDef(def.rest._def, {
        ...refs,
        currentPath: [...refs.currentPath, "additionalItems"]
      })
    };
  } else {
    return {
      type: "array",
      minItems: def.items.length,
      maxItems: def.items.length,
      items: def.items.map((x3, i7) => parseDef(x3._def, {
        ...refs,
        currentPath: [...refs.currentPath, "items", `${i7}`]
      })).reduce((acc, x3) => x3 === void 0 ? acc : [...acc, x3], [])
    };
  }
}

// node_modules/zod-to-json-schema/dist/esm/parsers/undefined.js
function parseUndefinedDef(refs) {
  return {
    not: parseAnyDef(refs)
  };
}

// node_modules/zod-to-json-schema/dist/esm/parsers/unknown.js
function parseUnknownDef(refs) {
  return parseAnyDef(refs);
}

// node_modules/zod-to-json-schema/dist/esm/parsers/readonly.js
var parseReadonlyDef = (def, refs) => {
  return parseDef(def.innerType._def, refs);
};

// node_modules/zod-to-json-schema/dist/esm/selectParser.js
var selectParser = (def, typeName, refs) => {
  switch (typeName) {
    case ZodFirstPartyTypeKind.ZodString:
      return parseStringDef(def, refs);
    case ZodFirstPartyTypeKind.ZodNumber:
      return parseNumberDef(def, refs);
    case ZodFirstPartyTypeKind.ZodObject:
      return parseObjectDef(def, refs);
    case ZodFirstPartyTypeKind.ZodBigInt:
      return parseBigintDef(def, refs);
    case ZodFirstPartyTypeKind.ZodBoolean:
      return parseBooleanDef();
    case ZodFirstPartyTypeKind.ZodDate:
      return parseDateDef(def, refs);
    case ZodFirstPartyTypeKind.ZodUndefined:
      return parseUndefinedDef(refs);
    case ZodFirstPartyTypeKind.ZodNull:
      return parseNullDef(refs);
    case ZodFirstPartyTypeKind.ZodArray:
      return parseArrayDef(def, refs);
    case ZodFirstPartyTypeKind.ZodUnion:
    case ZodFirstPartyTypeKind.ZodDiscriminatedUnion:
      return parseUnionDef(def, refs);
    case ZodFirstPartyTypeKind.ZodIntersection:
      return parseIntersectionDef(def, refs);
    case ZodFirstPartyTypeKind.ZodTuple:
      return parseTupleDef(def, refs);
    case ZodFirstPartyTypeKind.ZodRecord:
      return parseRecordDef(def, refs);
    case ZodFirstPartyTypeKind.ZodLiteral:
      return parseLiteralDef(def, refs);
    case ZodFirstPartyTypeKind.ZodEnum:
      return parseEnumDef(def);
    case ZodFirstPartyTypeKind.ZodNativeEnum:
      return parseNativeEnumDef(def);
    case ZodFirstPartyTypeKind.ZodNullable:
      return parseNullableDef(def, refs);
    case ZodFirstPartyTypeKind.ZodOptional:
      return parseOptionalDef(def, refs);
    case ZodFirstPartyTypeKind.ZodMap:
      return parseMapDef(def, refs);
    case ZodFirstPartyTypeKind.ZodSet:
      return parseSetDef(def, refs);
    case ZodFirstPartyTypeKind.ZodLazy:
      return () => def.getter()._def;
    case ZodFirstPartyTypeKind.ZodPromise:
      return parsePromiseDef(def, refs);
    case ZodFirstPartyTypeKind.ZodNaN:
    case ZodFirstPartyTypeKind.ZodNever:
      return parseNeverDef(refs);
    case ZodFirstPartyTypeKind.ZodEffects:
      return parseEffectsDef(def, refs);
    case ZodFirstPartyTypeKind.ZodAny:
      return parseAnyDef(refs);
    case ZodFirstPartyTypeKind.ZodUnknown:
      return parseUnknownDef(refs);
    case ZodFirstPartyTypeKind.ZodDefault:
      return parseDefaultDef(def, refs);
    case ZodFirstPartyTypeKind.ZodBranded:
      return parseBrandedDef(def, refs);
    case ZodFirstPartyTypeKind.ZodReadonly:
      return parseReadonlyDef(def, refs);
    case ZodFirstPartyTypeKind.ZodCatch:
      return parseCatchDef(def, refs);
    case ZodFirstPartyTypeKind.ZodPipeline:
      return parsePipelineDef(def, refs);
    case ZodFirstPartyTypeKind.ZodFunction:
    case ZodFirstPartyTypeKind.ZodVoid:
    case ZodFirstPartyTypeKind.ZodSymbol:
      return void 0;
    default:
      return /* @__PURE__ */ ((_3) => void 0)(typeName);
  }
};

// node_modules/zod-to-json-schema/dist/esm/parseDef.js
function parseDef(def, refs, forceResolution = false) {
  const seenItem = refs.seen.get(def);
  if (refs.override) {
    const overrideResult = refs.override?.(def, refs, seenItem, forceResolution);
    if (overrideResult !== ignoreOverride) {
      return overrideResult;
    }
  }
  if (seenItem && !forceResolution) {
    const seenSchema = get$ref(seenItem, refs);
    if (seenSchema !== void 0) {
      return seenSchema;
    }
  }
  const newItem = { def, path: refs.currentPath, jsonSchema: void 0 };
  refs.seen.set(def, newItem);
  const jsonSchemaOrGetter = selectParser(def, def.typeName, refs);
  const jsonSchema = typeof jsonSchemaOrGetter === "function" ? parseDef(jsonSchemaOrGetter(), refs) : jsonSchemaOrGetter;
  if (jsonSchema) {
    addMeta(def, refs, jsonSchema);
  }
  if (refs.postProcess) {
    const postProcessResult = refs.postProcess(jsonSchema, def, refs);
    newItem.jsonSchema = jsonSchema;
    return postProcessResult;
  }
  newItem.jsonSchema = jsonSchema;
  return jsonSchema;
}
var get$ref = (item, refs) => {
  switch (refs.$refStrategy) {
    case "root":
      return { $ref: item.path.join("/") };
    case "relative":
      return { $ref: getRelativePath(refs.currentPath, item.path) };
    case "none":
    case "seen": {
      if (item.path.length < refs.currentPath.length && item.path.every((value, index) => refs.currentPath[index] === value)) {
        console.warn(`Recursive reference detected at ${refs.currentPath.join("/")}! Defaulting to any`);
        return parseAnyDef(refs);
      }
      return refs.$refStrategy === "seen" ? parseAnyDef(refs) : void 0;
    }
  }
};
var addMeta = (def, refs, jsonSchema) => {
  if (def.description) {
    jsonSchema.description = def.description;
    if (refs.markdownDescription) {
      jsonSchema.markdownDescription = def.description;
    }
  }
  return jsonSchema;
};

// node_modules/zod-to-json-schema/dist/esm/zodToJsonSchema.js
var zodToJsonSchema = (schema, options) => {
  const refs = getRefs(options);
  let definitions2 = typeof options === "object" && options.definitions ? Object.entries(options.definitions).reduce((acc, [name2, schema2]) => ({
    ...acc,
    [name2]: parseDef(schema2._def, {
      ...refs,
      currentPath: [...refs.basePath, refs.definitionPath, name2]
    }, true) ?? parseAnyDef(refs)
  }), {}) : void 0;
  const name = typeof options === "string" ? options : options?.nameStrategy === "title" ? void 0 : options?.name;
  const main = parseDef(schema._def, name === void 0 ? refs : {
    ...refs,
    currentPath: [...refs.basePath, refs.definitionPath, name]
  }, false) ?? parseAnyDef(refs);
  const title = typeof options === "object" && options.name !== void 0 && options.nameStrategy === "title" ? options.name : void 0;
  if (title !== void 0) {
    main.title = title;
  }
  if (refs.flags.hasReferencedOpenAiAnyType) {
    if (!definitions2) {
      definitions2 = {};
    }
    if (!definitions2[refs.openAiAnyTypeName]) {
      definitions2[refs.openAiAnyTypeName] = {
        // Skipping "object" as no properties can be defined and additionalProperties must be "false"
        type: ["string", "number", "integer", "boolean", "array", "null"],
        items: {
          $ref: refs.$refStrategy === "relative" ? "1" : [
            ...refs.basePath,
            refs.definitionPath,
            refs.openAiAnyTypeName
          ].join("/")
        }
      };
    }
  }
  const combined = name === void 0 ? definitions2 ? {
    ...main,
    [refs.definitionPath]: definitions2
  } : main : {
    $ref: [
      ...refs.$refStrategy === "relative" ? [] : refs.basePath,
      refs.definitionPath,
      name
    ].join("/"),
    [refs.definitionPath]: {
      ...definitions2,
      [name]: main
    }
  };
  if (refs.target === "jsonSchema7") {
    combined.$schema = "http://json-schema.org/draft-07/schema#";
  } else if (refs.target === "jsonSchema2019-09" || refs.target === "openAi") {
    combined.$schema = "https://json-schema.org/draft/2019-09/schema#";
  }
  if (refs.target === "openAi" && ("anyOf" in combined || "oneOf" in combined || "allOf" in combined || "type" in combined && Array.isArray(combined.type))) {
    console.warn("Warning: OpenAI may not support schemas with unions as roots! Try wrapping it in an object property.");
  }
  return combined;
};

// node_modules/@a2ui/web_core/src/errors.js
var A2uiError = class extends Error {
  /**
   * Initializes a new `A2uiError` instance.
   *
   * @param message Human-readable error description.
   * @param code Machine-readable error category code.
   * @param context Optional error context metadata (path, surfaceId, functionCallId).
   */
  constructor(message, code = "UNKNOWN_ERROR", context) {
    super(message);
    this.name = this.constructor.name;
    this.code = code;
    if (context?.path !== void 0)
      this.path = context.path;
    if (context?.surfaceId !== void 0)
      this.surfaceId = context.surfaceId;
    if (context?.functionCallId !== void 0) {
      this.functionCallId = context.functionCallId;
    }
    if (Error.captureStackTrace) {
      Error.captureStackTrace(this, this.constructor);
    }
  }
};
var A2uiValidationError = class extends A2uiError {
  /**
   * Initializes a new `A2uiValidationError` instance.
   *
   * @param message Error description detailing the validation failure.
   * @param details Additional error context or Zod validation issues.
   * @param code Optional error category code.
   * @param context Optional error context metadata.
   */
  constructor(message, details, code = "VALIDATION_ERROR", context) {
    super(message, code, context);
    this.details = details;
  }
};
var A2uiDataError = class extends A2uiError {
  /**
   * Initializes a new `A2uiDataError` instance.
   *
   * @param message Error description.
   * @param path Target data model path where the mutation failed.
   * @param surfaceId Optional target surface identifier.
   */
  constructor(message, path, surfaceId) {
    super(message, "DATA_ERROR", { path, surfaceId });
  }
};
var A2uiExpressionError = class extends A2uiError {
  /**
   * Initializes a new `A2uiExpressionError` instance.
   *
   * @param message Error description.
   * @param expression Evaluated expression string.
   * @param details Additional error details.
   * @param context Optional error context metadata.
   */
  constructor(message, expression, details, context) {
    super(message, "EXPRESSION_ERROR", context);
    this.expression = expression;
    this.details = details;
  }
};
var A2uiStateError = class extends A2uiError {
  /**
   * Initializes a new `A2uiStateError` instance.
   *
   * @param message Error description.
   * @param context Optional error context metadata.
   */
  constructor(message, context) {
    super(message, "STATE_ERROR", context);
  }
};
var A2uiCatalogError = class extends A2uiError {
  /**
   * Initializes a new `A2uiCatalogError` instance.
   *
   * @param message Human-readable error description.
   * @param code Machine-readable error category code. Defaults to `'CATALOG_ERROR'`.
   * @param context Optional error context metadata.
   */
  constructor(message, code = "CATALOG_ERROR", context) {
    super(message, code, context);
  }
};
var A2uiIntegrityError = class extends A2uiValidationError {
  /**
   * Initializes a new `A2uiIntegrityError` instance.
   *
   * @param message Error description.
   * @param details Additional error details.
   * @param code Error category code. Defaults to 'INTEGRITY_ERROR'.
   * @param context Optional error context metadata.
   */
  constructor(message, details, code = "INTEGRITY_ERROR", context) {
    super(message, details, code, context);
    this.name = "A2uiIntegrityError";
  }
};
var A2uiRecursionError = class extends A2uiValidationError {
  /**
   * Initializes a new `A2uiRecursionError` instance.
   *
   * @param message Error description.
   * @param details Additional error details.
   * @param code Error category code. Defaults to 'RECURSION_ERROR'.
   * @param context Optional error context metadata.
   */
  constructor(message, details, code = "RECURSION_ERROR", context) {
    super(message, details, code, context);
    this.name = "A2uiRecursionError";
  }
};
var RpcErrorCode;
(function(RpcErrorCode2) {
  RpcErrorCode2["INVALID_FUNCTION_CALL"] = "INVALID_FUNCTION_CALL";
  RpcErrorCode2["EXECUTION_ERROR"] = "EXECUTION_ERROR";
  RpcErrorCode2["UNKNOWN_FUNCTION"] = "UNKNOWN_FUNCTION";
  RpcErrorCode2["UNKNOWN_ERROR"] = "UNKNOWN_ERROR";
  RpcErrorCode2["TIMEOUT"] = "TIMEOUT";
  RpcErrorCode2["CANCELLED"] = "CANCELLED";
  RpcErrorCode2["DISPOSED"] = "DISPOSED";
  RpcErrorCode2["DUPLICATE"] = "DUPLICATE";
  RpcErrorCode2["NO_LISTENER"] = "NO_LISTENER";
})(RpcErrorCode || (RpcErrorCode = {}));
var A2uiRpcError = class extends A2uiError {
  /**
   * Initializes a new `A2uiRpcError` instance.
   *
   * @param message Human-readable error description.
   * @param code Category code or string identifying the RPC error.
   * @param functionCallId Optional identifier of the failed function call.
   * @param details Optional structured error details.
   */
  constructor(message, code = RpcErrorCode.UNKNOWN_ERROR, functionCallId, details) {
    const resolvedCode = code || RpcErrorCode.UNKNOWN_ERROR;
    super(`[${resolvedCode}] ${message}`, resolvedCode);
    this.functionCallId = functionCallId;
    this.details = details;
    this.name = "A2uiRpcError";
  }
};

// node_modules/@preact/signals-core/dist/signals-core.module.js
var i = /* @__PURE__ */ Symbol.for("preact-signals");
function t() {
  if (!(v > 1)) {
    var i7, t7 = false;
    !(function() {
      var i8 = c;
      c = void 0;
      while (void 0 !== i8) {
        var t8 = i8.S;
        if (t8.v === i8.v) {
          for (var r9 = t8.t; void 0 !== r9; r9 = r9.x) if (r9.i === i8.i) r9.i = t8.i;
        }
        i8 = i8.o;
      }
    })();
    while (void 0 !== h) {
      var r8 = h;
      h = void 0;
      s++;
      while (void 0 !== r8) {
        var n6 = r8.u;
        r8.u = void 0;
        r8.f &= -3;
        if (!(8 & r8.f) && w(r8)) try {
          r8.c();
        } catch (r9) {
          if (!t7) {
            i7 = r9;
            t7 = true;
          }
        }
        r8 = n6;
      }
    }
    s = 0;
    v--;
    if (t7) throw i7;
  } else v--;
}
function r(i7) {
  if (v > 0) return i7();
  u = ++e;
  v++;
  try {
    return i7();
  } finally {
    t();
  }
}
var n;
var o = void 0;
function f(i7) {
  var t7 = o, r8 = n;
  o = void 0;
  n = void 0;
  try {
    return i7();
  } finally {
    o = t7;
    n = r8;
  }
}
var h = void 0;
var v = 0;
var s = 0;
var e = 0;
var u = 0;
var c = void 0;
var a = 0;
function d(i7) {
  if (void 0 !== o) {
    var t7 = i7.n;
    if (void 0 === t7 || t7.t !== o) {
      t7 = { i: 0, S: i7, p: o.s, n: void 0, t: o, e: void 0, x: void 0, r: t7 };
      if (void 0 !== o.s) o.s.n = t7;
      o.s = t7;
      i7.n = t7;
      if (32 & o.f) i7.S(t7);
      return t7;
    } else if (-1 === t7.i) {
      t7.i = 0;
      if (void 0 !== t7.n) {
        t7.n.p = t7.p;
        if (void 0 !== t7.p) t7.p.n = t7.n;
        t7.p = o.s;
        t7.n = void 0;
        o.s.n = t7;
        o.s = t7;
      }
      return t7;
    }
  }
}
function l(i7, t7) {
  this.v = i7;
  this.i = 0;
  this.n = void 0;
  this.t = void 0;
  this.l = 0;
  this.W = null == t7 ? void 0 : t7.watched;
  this.Z = null == t7 ? void 0 : t7.unwatched;
  this.name = null == t7 ? void 0 : t7.name;
}
l.prototype.brand = i;
l.prototype.h = function() {
  return true;
};
l.prototype.S = function(i7) {
  var t7 = this, r8 = this.t;
  if (r8 !== i7 && void 0 === i7.e) {
    i7.x = r8;
    this.t = i7;
    if (void 0 !== r8) r8.e = i7;
    else f(function() {
      var i8;
      null == (i8 = t7.W) || i8.call(t7);
    });
  }
};
l.prototype.U = function(i7) {
  var t7 = this;
  if (void 0 !== this.t) {
    var r8 = i7.e, n6 = i7.x;
    if (void 0 !== r8) {
      r8.x = n6;
      i7.e = void 0;
    }
    if (void 0 !== n6) {
      n6.e = r8;
      i7.x = void 0;
    }
    if (i7 === this.t) {
      this.t = n6;
      if (void 0 === n6) f(function() {
        var i8;
        null == (i8 = t7.Z) || i8.call(t7);
      });
    }
  }
};
l.prototype.subscribe = function(i7) {
  var t7 = this;
  return x(function() {
    var r8 = t7.value;
    f(function() {
      return i7(r8);
    });
  }, { name: "sub" });
};
l.prototype.valueOf = function() {
  return this.value;
};
l.prototype.toString = function() {
  return this.value + "";
};
l.prototype.toJSON = function() {
  return this.value;
};
l.prototype.peek = function() {
  var i7 = this;
  return f(function() {
    return i7.value;
  });
};
Object.defineProperty(l.prototype, "value", { get: function() {
  var i7 = d(this);
  if (void 0 !== i7) i7.i = this.i;
  return this.v;
}, set: function(i7) {
  if (i7 !== this.v) {
    if (s > 100) throw new Error("Cycle detected");
    !(function(i8) {
      if (0 !== v && 0 === s) {
        if (i8.l !== u) {
          i8.l = u;
          c = { S: i8, v: i8.v, i: i8.i, o: c };
        }
      }
    })(this);
    this.v = i7;
    this.i++;
    a++;
    v++;
    try {
      for (var r8 = this.t; void 0 !== r8; r8 = r8.x) r8.t.N();
    } finally {
      t();
    }
  }
} });
function y(i7, t7) {
  return new l(i7, t7);
}
function w(i7) {
  for (var t7 = i7.s; void 0 !== t7; t7 = t7.n) if (t7.S.i !== t7.i || !t7.S.h() || t7.S.i !== t7.i) return true;
  return false;
}
function b(i7) {
  for (var t7 = i7.s; void 0 !== t7; t7 = t7.n) {
    var r8 = t7.S.n;
    if (void 0 !== r8) t7.r = r8;
    t7.S.n = t7;
    t7.i = -1;
    if (void 0 === t7.n) {
      i7.s = t7;
      break;
    }
  }
}
function _(i7) {
  var t7 = i7.s, r8 = void 0;
  while (void 0 !== t7) {
    var n6 = t7.p;
    if (-1 === t7.i) {
      t7.S.U(t7);
      if (void 0 !== n6) n6.n = t7.n;
      if (void 0 !== t7.n) t7.n.p = n6;
    } else r8 = t7;
    t7.S.n = t7.r;
    if (void 0 !== t7.r) t7.r = void 0;
    t7 = n6;
  }
  i7.s = r8;
}
function j(i7, t7) {
  l.call(this, void 0, t7);
  this.x = i7;
  this.s = void 0;
  this.g = a - 1;
  this.f = 4;
}
j.prototype = new l();
j.prototype.h = function() {
  this.f &= -3;
  if (1 & this.f) return false;
  if (32 == (36 & this.f)) return true;
  this.f &= -5;
  if (this.g === a) return true;
  this.g = a;
  this.f |= 1;
  if (this.i > 0 && !w(this)) {
    this.f &= -2;
    return true;
  }
  var i7 = o;
  try {
    b(this);
    o = this;
    var t7 = this.x();
    if (16 & this.f || this.v !== t7 || 0 === this.i) {
      this.v = t7;
      this.f &= -17;
      this.i++;
    }
  } catch (i8) {
    this.v = i8;
    this.f |= 16;
    this.i++;
  }
  o = i7;
  _(this);
  this.f &= -2;
  return true;
};
j.prototype.S = function(i7) {
  if (void 0 === this.t) {
    this.f |= 36;
    for (var t7 = this.s; void 0 !== t7; t7 = t7.n) t7.S.S(t7);
  }
  l.prototype.S.call(this, i7);
};
j.prototype.U = function(i7) {
  if (void 0 !== this.t) {
    l.prototype.U.call(this, i7);
    if (void 0 === this.t) {
      this.f &= -33;
      for (var t7 = this.s; void 0 !== t7; t7 = t7.n) t7.S.U(t7);
    }
  }
};
j.prototype.N = function() {
  if (!(2 & this.f)) {
    this.f |= 6;
    for (var i7 = this.t; void 0 !== i7; i7 = i7.x) i7.t.N();
  }
};
Object.defineProperty(j.prototype, "value", { get: function() {
  if (1 & this.f) throw new Error("Cycle detected");
  var i7 = d(this);
  this.h();
  if (void 0 !== i7) i7.i = this.i;
  if (16 & this.f) throw this.v;
  return this.v;
} });
function O(i7, t7) {
  return new j(i7, t7);
}
function p(i7) {
  var r8 = i7.j;
  i7.j = void 0;
  if ("function" == typeof r8) {
    v++;
    var n6 = o;
    o = void 0;
    try {
      r8();
    } catch (t7) {
      i7.f &= -2;
      i7.f |= 8;
      g(i7);
      throw t7;
    } finally {
      o = n6;
      t();
    }
  }
}
function g(i7) {
  for (var t7 = i7.s; void 0 !== t7; t7 = t7.n) t7.S.U(t7);
  i7.x = void 0;
  i7.s = void 0;
  p(i7);
}
function S(i7) {
  if (o !== this) throw new Error("Out-of-order effect");
  _(this);
  o = i7;
  this.f &= -2;
  try {
    if (8 & this.f) g(this);
  } finally {
    t();
  }
}
function m(i7, t7) {
  this.x = i7;
  this.j = void 0;
  this.s = void 0;
  this.u = void 0;
  this.f = 32;
  this.name = null == t7 ? void 0 : t7.name;
  if (n) n.push(this);
}
m.prototype.c = function() {
  var i7 = this.S();
  try {
    if (8 & this.f) return;
    if (void 0 === this.x) return;
    var t7 = this.x();
    if ("function" == typeof t7) this.j = t7;
  } finally {
    i7();
  }
};
m.prototype.S = function() {
  if (1 & this.f) throw new Error("Cycle detected");
  this.f |= 1;
  this.f &= -9;
  p(this);
  b(this);
  v++;
  var i7 = o;
  o = this;
  return S.bind(this, i7);
};
m.prototype.N = function() {
  if (!(2 & this.f)) {
    this.f |= 2;
    this.u = h;
    h = this;
  }
};
m.prototype.d = function() {
  this.f |= 8;
  if (!(1 & this.f)) g(this);
};
m.prototype.dispose = function() {
  this.d();
};
function x(i7, t7) {
  var r8 = new m(i7, t7);
  try {
    r8.c();
  } catch (i8) {
    r8.d();
    throw i8;
  }
  var n6 = r8.d.bind(r8);
  n6[Symbol.dispose] = n6;
  return n6;
}

// node_modules/@a2ui/web_core/src/reactivity/signals.js
var signalImpl;
var computedImpl;
var effectImpl;
var batchWriteImpl;
var isSignalImpl;
var getValueImpl;
var setValueImpl;
var peekValueImpl;
var _PRIVATE_DEFAULT_SIGNAL_IMPLEMENTATION = {
  signal: y,
  computed: O,
  effect: x,
  batchWrite: r,
  isSignal: (val) => typeof val === "function" || !!val && typeof val === "object" && ("value" in val || "peek" in val),
  getValue: (signal2) => {
    if (typeof signal2 === "function") {
      return signal2();
    }
    return signal2.value;
  },
  setValue: (signal2, value) => {
    if (typeof signal2?.set === "function") {
      signal2.set(value);
      return;
    }
    if (signal2 instanceof j || typeof signal2 === "function") {
      throw new TypeError("Cannot set value of a read-only computed signal.");
    }
    signal2.value = value;
  },
  peekValue: (signal2) => {
    if (typeof signal2?.peek === "function") {
      return signal2.peek();
    }
    if (typeof signal2 === "function") {
      return signal2();
    }
    if (signal2 && typeof signal2 === "object" && "value" in signal2) {
      return signal2.value;
    }
    return signal2;
  }
};
setSignalImplementation(_PRIVATE_DEFAULT_SIGNAL_IMPLEMENTATION);
function setSignalImplementation(impl) {
  signalImpl = impl.signal;
  computedImpl = impl.computed;
  effectImpl = impl.effect;
  batchWriteImpl = impl.batchWrite;
  isSignalImpl = impl.isSignal;
  getValueImpl = impl.getValue;
  setValueImpl = impl.setValue;
  peekValueImpl = impl.peekValue;
}
function signal(initialValue) {
  return signalImpl(initialValue);
}
function computed(computeFn) {
  return computedImpl(computeFn);
}
function effect(effectFn) {
  return effectImpl(effectFn);
}
function batchWrite(batchFn) {
  return batchWriteImpl(batchFn);
}
function isSignal(val) {
  return isSignalImpl(val);
}
function getValue(signal2) {
  return getValueImpl(signal2);
}
function setValue(signal2, value) {
  setValueImpl(signal2, value);
}
function peekValue(signal2) {
  return peekValueImpl(signal2);
}

// node_modules/@a2ui/web_core/src/state/data-model.js
function isNumeric(value) {
  return /^(0|[1-9]\d*)$/.test(value);
}
var MAX_ARRAY_INDEX = 1e4;
var FORBIDDEN_KEYS = /* @__PURE__ */ new Set(["__proto__", "constructor", "prototype"]);
var DataModel = class {
  /**
   * Initializes a new `DataModel` instance.
   *
   * @param initialData Initial data for the model. Defaults to an empty object.
   */
  constructor(initialData = {}) {
    this.data = {};
    this.signals = /* @__PURE__ */ new Map();
    this.subscriptions = /* @__PURE__ */ new Set();
    this.data = initialData;
  }
  /**
   * Retrieves a Preact Signal for a specific data path.
   *
   * This provides a reactive way to access a value. If the value at the path
   * changes via `set()`, the signal will automatically be updated.
   *
   * @param path The JSON pointer path to create or retrieve a signal for.
   * @returns A Preact Signal representing the value at the specified path.
   * @throws {A2uiDataError} If path is null, undefined, or contains forbidden
   *     segments (`__proto__`, `constructor`, `prototype`).
   */
  getSignal(path) {
    const normalizedPath = this.normalizePath(path);
    if (!this.signals.has(normalizedPath)) {
      this.signals.set(normalizedPath, signal(this.get(normalizedPath)));
    }
    return this.signals.get(normalizedPath);
  }
  /**
   * Updates the model at the specific path and notifies all relevant signals.
   * If path is '/' or empty, replaces the entire root.
   *
   * Note on `undefined` values:
   * - For objects: Setting a property to `undefined` removes the key from the
   * object.
   * - For arrays: Setting an index to `undefined` sets that index to
   * `undefined` but preserves the array length (sparse array).
   *
   * @param path The JSON pointer path to set value for.
   * @param value The value to set at the specified path.
   * @param preserveNull Whether to store `null` as a value instead of treating
   *     it like `undefined`. Pre-1.0 data models stored `null`; the v1.0
   *     protocol removes the entry.
   * @returns This `DataModel` instance for chaining.
   * @throws {A2uiDataError} If path is null, undefined, invalid for
   *     arrays/primitives, exceeds max array bounds, or contains forbidden
   *     segments (`__proto__`, `constructor`, `prototype`).
   */
  set(path, value, preserveNull = false) {
    if (path === null || path === void 0) {
      throw new A2uiDataError("Path cannot be null or undefined.");
    }
    const segments = this.parsePath(path);
    if (segments.length === 0) {
      this.data = value === null || value === void 0 ? {} : value;
      this.notifyAllSignals();
      return this;
    }
    const lastSegment = segments.pop();
    const removes = value === void 0 || !preserveNull && value === null;
    if (removes && this.get(path) === void 0) {
      return this;
    }
    if (this.data === void 0 || this.data === null) {
      this.data = {};
    } else if (typeof this.data !== "object") {
      throw new A2uiDataError(`Cannot set path '${path}': the data model root is a primitive value.`, path);
    }
    let current = this.data;
    for (let i7 = 0; i7 < segments.length; i7++) {
      const segment = segments[i7];
      if (Array.isArray(current)) {
        if (!isNumeric(segment)) {
          throw new A2uiDataError(`Cannot use non-numeric segment '${segment}' on an array in path '${path}'.`, path);
        }
        const index = parseInt(segment, 10);
        if (index > MAX_ARRAY_INDEX) {
          throw new A2uiDataError(`Cannot set path '${path}': array index '${segment}' exceeds maximum supported index (${MAX_ARRAY_INDEX}).`, path);
        }
        const val = current[index];
        if (val !== void 0 && val !== null && typeof val !== "object") {
          throw new A2uiDataError(`Cannot set path '${path}': segment '${segment}' is a primitive value.`, path);
        }
        if (val === void 0 || val === null) {
          const nextSegment = i7 < segments.length - 1 ? segments[i7 + 1] : lastSegment;
          if (isNumeric(nextSegment)) {
            const nextIdx = parseInt(nextSegment, 10);
            if (nextIdx > MAX_ARRAY_INDEX) {
              throw new A2uiDataError(`Cannot set path '${path}': array index '${nextSegment}' exceeds maximum supported index (${MAX_ARRAY_INDEX}).`, path);
            }
            current[index] = [];
          } else {
            current[index] = {};
          }
        }
      } else {
        const hasOwnProp = Object.prototype.hasOwnProperty.call(current, segment);
        if (hasOwnProp) {
          const propVal = current[segment];
          if (propVal !== void 0 && propVal !== null && typeof propVal !== "object") {
            throw new A2uiDataError(`Cannot set path '${path}': segment '${segment}' is a primitive value.`, path);
          }
        }
        if (!hasOwnProp || current[segment] === void 0 || current[segment] === null) {
          const nextSegment = i7 < segments.length - 1 ? segments[i7 + 1] : lastSegment;
          if (isNumeric(nextSegment)) {
            const nextIdx = parseInt(nextSegment, 10);
            if (nextIdx > MAX_ARRAY_INDEX) {
              throw new A2uiDataError(`Cannot set path '${path}': array index '${nextSegment}' exceeds maximum supported index (${MAX_ARRAY_INDEX}).`, path);
            }
            current[segment] = [];
          } else {
            current[segment] = {};
          }
        }
      }
      current = current[segment];
    }
    if (Array.isArray(current)) {
      if (!isNumeric(lastSegment)) {
        throw new A2uiDataError(`Cannot use non-numeric segment '${lastSegment}' on an array in path '${path}'.`, path);
      }
      const lastIndex = parseInt(lastSegment, 10);
      if (lastIndex > MAX_ARRAY_INDEX) {
        throw new A2uiDataError(`Cannot set path '${path}': array index '${lastSegment}' exceeds maximum supported index (${MAX_ARRAY_INDEX}).`, path);
      }
    }
    if (removes) {
      if (Array.isArray(current)) {
        const idx = parseInt(lastSegment, 10);
        if (idx < current.length) {
          current[idx] = void 0;
        }
      } else {
        delete current[lastSegment];
      }
    } else {
      current[lastSegment] = value;
    }
    this.notifySignals(path);
    return this;
  }
  /**
   * Deletes the value at the specified JSON pointer path.
   *
   * If `path` is `'/'` or empty, resets the root data model to an empty object.
   *
   * @param path The JSON pointer path to remove.
   * @returns This `DataModel` instance for chaining.
   */
  delete(path) {
    return this.set(path, void 0);
  }
  /**
   * Resolves a relative path against a base context path.
   *
   * @param path The path to resolve.
   * @param contextPath The base path (optional).
   * @returns The resolved absolute path.
   */
  static resolvePath(path, contextPath) {
    if (path.startsWith("/")) {
      return path;
    }
    if (contextPath) {
      const base = contextPath.endsWith("/") ? contextPath : `${contextPath}/`;
      return `${base}${path}`;
    }
    return `/${path}`;
  }
  /**
   * Retrieves data at a specific JSON pointer path.
   *
   * @template T The expected type of the returned value. Defaults to `any`.
   * @param path The JSON pointer path to read from.
   * @returns The value at the specified path, or undefined if not found.
   * @throws {A2uiDataError} If path is null, undefined, or contains forbidden
   *     segments (`__proto__`, `constructor`, `prototype`).
   */
  get(path) {
    if (path === null || path === void 0) {
      throw new A2uiDataError("Path cannot be null or undefined.");
    }
    if (path === "/" || path === "") {
      return this.data;
    }
    const segments = this.parsePath(path);
    let current = this.data;
    for (const segment of segments) {
      if (current === void 0 || current === null || typeof current !== "object") {
        return void 0;
      }
      if (Array.isArray(current)) {
        if (!isNumeric(segment)) {
          return void 0;
        }
        const index = parseInt(segment, 10);
        if (index < 0 || index >= current.length) {
          return void 0;
        }
        current = current[index];
      } else {
        if (!Object.prototype.hasOwnProperty.call(current, segment)) {
          return void 0;
        }
        current = current[segment];
      }
    }
    return current;
  }
  /**
   * Checks whether a JSON pointer path physically exists in the data model.
   *
   * Differentiates between a path holding an explicit `null` value (`true`) and
   * a path that is absent from the object or array hierarchy (`false`).
   *
   * @param path Absolute JSON pointer path to check.
   * @returns Whether every segment along `path` exists in the hierarchy.
   */
  hasPath(path) {
    if (path === null || path === void 0) {
      return false;
    }
    return this.get(path) !== void 0;
  }
  /**
   * Subscribes to changes at the specified data path.
   *
   * This is a backwards-compatible layer using Preact Signals internally. It
   * allows listeners to be notified whenever the value at the specified path
   * (or any of its ancestors/descendants) changes.
   *
   * @param path The JSON pointer path to observe.
   * @param onChange A callback fired whenever the value changes.
   * @returns A `DataSubscription` containing the initial value and an
   *     `unsubscribe` method.
   * @throws {A2uiDataError} If path is null, undefined, or contains forbidden
   *     segments (`__proto__`, `constructor`, `prototype`).
   */
  subscribe(path, onChange) {
    const sig = this.getSignal(path);
    let isSync = true;
    let currentValue = peekValue(sig);
    const dispose = effect(() => {
      const val = getValue(sig);
      currentValue = val;
      if (!isSync) {
        onChange(val);
      }
    });
    isSync = false;
    this.subscriptions.add(dispose);
    return {
      get value() {
        return currentValue;
      },
      unsubscribe: () => {
        dispose();
        this.subscriptions.delete(dispose);
      }
    };
  }
  /**
   * Clears all internal subscriptions and active signals.
   */
  dispose() {
    for (const dispose of this.subscriptions) {
      dispose();
    }
    this.subscriptions.clear();
    this.signals.clear();
  }
  normalizePath(path) {
    const segments = path.split("/").filter((p5) => p5.length > 0);
    return segments.length === 0 ? "/" : `/${segments.join("/")}`;
  }
  parsePath(path) {
    return path.split("/").filter((p5) => p5.length > 0).map((rawSegment) => {
      const segment = rawSegment.replace(/~([01])/g, (_3, g3) => g3 === "1" ? "/" : "~");
      if (FORBIDDEN_KEYS.has(segment)) {
        throw new A2uiDataError(`Forbidden path segment '${segment}' in path '${path}'.`, path);
      }
      return segment;
    });
  }
  notifySignals(path) {
    const normalizedPath = this.normalizePath(path);
    batchWrite(() => {
      this.updateSignal(normalizedPath);
      let parentPath = normalizedPath;
      while (parentPath !== "/" && parentPath !== "") {
        parentPath = parentPath.substring(0, parentPath.lastIndexOf("/")) || "/";
        this.updateSignal(parentPath);
      }
      for (const subPath of this.signals.keys()) {
        if (this.isDescendant(subPath, normalizedPath)) {
          this.updateSignal(subPath);
        }
      }
    });
  }
  updateSignal(path) {
    const sig = this.signals.get(path);
    if (sig) {
      const val = this.get(path);
      if (Array.isArray(val)) {
        setValue(sig, val.slice());
      } else if (typeof val === "object" && val !== null) {
        setValue(sig, { ...val });
      } else {
        setValue(sig, val);
      }
    }
  }
  notifyAllSignals() {
    batchWrite(() => {
      for (const path of this.signals.keys()) {
        this.updateSignal(path);
      }
    });
  }
  isDescendant(childPath, parentPath) {
    if (parentPath === "/" || parentPath === "") {
      return childPath !== "/";
    }
    return childPath.startsWith(parentPath + "/");
  }
};

// node_modules/@a2ui/web_core/src/common/events.js
var EventEmitter = class {
  constructor() {
    this.listeners = /* @__PURE__ */ new Set();
  }
  /**
   * Subscribes to the event.
   *
   * @param listener The listener function to call when the event is emitted.
   * @returns A subscription object that can be used to unsubscribe.
   */
  subscribe(listener) {
    this.listeners.add(listener);
    return {
      unsubscribe: () => this.listeners.delete(listener)
    };
  }
  /**
   * Emits an event to all subscribers.
   *
   * @param data The data to pass to subscribers.
   */
  async emit(data) {
    for (const listener of this.listeners) {
      try {
        const res = listener(data);
        if (res && typeof res.then === "function") {
          await res;
        }
      } catch (e7) {
        console.error("EventEmitter error:", e7);
      }
    }
  }
  /**
   * Removes all registered listeners and disposes the emitter.
   */
  dispose() {
    this.listeners.clear();
  }
};

// node_modules/@a2ui/web_core/src/common/semver.js
var SEMVER_REGEX = /^[vV]?(0|[1-9]\d*)\.(0|[1-9]\d*)(?:\.(0|[1-9]\d*))?(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$/;
function normalizeVersionString(version) {
  if (!version || typeof version !== "string") {
    return "";
  }
  const text = version.trim().replace(/^[vV]/, "");
  const dashIdx = text.indexOf("-");
  const plusIdx = text.indexOf("+");
  let splitIdx = text.length;
  if (dashIdx !== -1 && plusIdx !== -1) {
    splitIdx = Math.min(dashIdx, plusIdx);
  } else if (dashIdx !== -1) {
    splitIdx = dashIdx;
  } else if (plusIdx !== -1) {
    splitIdx = plusIdx;
  }
  const core = text.slice(0, splitIdx).replace(/_/g, ".");
  return `${core}${text.slice(splitIdx)}`;
}
function parseSemVer(versionStr) {
  if (!versionStr || typeof versionStr !== "string") {
    return null;
  }
  const match = versionStr.trim().match(SEMVER_REGEX);
  if (!match) {
    return null;
  }
  return {
    major: parseInt(match[1], 10),
    minor: parseInt(match[2], 10),
    patch: match[3] !== void 0 ? parseInt(match[3], 10) : 0,
    prerelease: match[4] ? match[4].split(".") : [],
    build: match[5] ? match[5].split(".") : []
  };
}
function toSemVer(v3) {
  if (!v3) {
    return null;
  }
  if (typeof v3 === "object") {
    const obj = v3;
    if (typeof obj.major !== "number" || !Number.isInteger(obj.major) || obj.major < 0 || typeof obj.minor === "number" && (!Number.isInteger(obj.minor) || obj.minor < 0) || typeof obj.patch === "number" && (!Number.isInteger(obj.patch) || obj.patch < 0)) {
      return null;
    }
    return {
      major: obj.major,
      minor: typeof obj.minor === "number" ? obj.minor : 0,
      patch: typeof obj.patch === "number" ? obj.patch : 0,
      prerelease: Array.isArray(obj.prerelease) ? obj.prerelease : [],
      build: Array.isArray(obj.build) ? obj.build : []
    };
  }
  return parseSemVer(normalizeVersionString(v3));
}
function toCanonicalVersion(version) {
  if (!version) {
    return null;
  }
  const parsed = toSemVer(version);
  if (!parsed) {
    return null;
  }
  if (parsed.patch === 0 && parsed.prerelease.length === 0 && parsed.build.length === 0) {
    return `${parsed.major}.${parsed.minor}`;
  }
  const pre = parsed.prerelease.length > 0 ? `-${parsed.prerelease.join(".")}` : "";
  const bld = parsed.build.length > 0 ? `+${parsed.build.join(".")}` : "";
  return `${parsed.major}.${parsed.minor}.${parsed.patch}${pre}${bld}`;
}
function comparePrereleaseId(idA, idB) {
  const isNumA = /^\d+$/.test(idA);
  const isNumB = /^\d+$/.test(idB);
  if (isNumA && isNumB) {
    if (idA.length !== idB.length) {
      return idA.length - idB.length;
    }
    if (idA < idB) {
      return -1;
    }
    if (idA > idB) {
      return 1;
    }
    return 0;
  }
  if (isNumA) {
    return -1;
  }
  if (isNumB) {
    return 1;
  }
  if (idA < idB) {
    return -1;
  }
  if (idA > idB) {
    return 1;
  }
  return 0;
}
function comparePrereleaseLists(preA, preB) {
  if (preA.length === 0 && preB.length === 0) {
    return 0;
  }
  if (preA.length === 0) {
    return 1;
  }
  if (preB.length === 0) {
    return -1;
  }
  const minLen = Math.min(preA.length, preB.length);
  for (let i7 = 0; i7 < minLen; i7++) {
    const diff = comparePrereleaseId(preA[i7], preB[i7]);
    if (diff !== 0) {
      return diff;
    }
  }
  return preA.length - preB.length;
}
function compareSemVer(a4, b4) {
  const versionA = toSemVer(a4);
  const versionB = toSemVer(b4);
  if (!versionA && !versionB) {
    return 0;
  }
  if (!versionA) {
    return -1;
  }
  if (!versionB) {
    return 1;
  }
  if (versionA.major !== versionB.major) {
    return versionA.major - versionB.major;
  }
  if (versionA.minor !== versionB.minor) {
    return versionA.minor - versionB.minor;
  }
  if (versionA.patch !== versionB.patch) {
    return versionA.patch - versionB.patch;
  }
  return comparePrereleaseLists(versionA.prerelease, versionB.prerelease);
}
function isAtLeastVersion(version, minVersion) {
  if (!version) {
    return false;
  }
  const parsed = toSemVer(version);
  const minParsed = toSemVer(minVersion);
  if (!parsed || !minParsed) {
    return false;
  }
  return compareSemVer(parsed, minParsed) >= 0;
}

// node_modules/@a2ui/web_core/src/common/settle_deep.js
function isThenable(value) {
  return typeof value === "object" && value !== null && typeof value.then === "function";
}
function isPlainObject(value) {
  if (Array.isArray(value) || typeof value !== "object" || value === null) {
    return false;
  }
  const proto = Object.getPrototypeOf(value);
  return proto === Object.prototype || proto === null;
}
function settleDeep(value) {
  if (isThenable(value)) {
    return Promise.resolve(value).then(settleDeep);
  }
  if (Array.isArray(value)) {
    const items = value.map(settleDeep);
    return items.some(isThenable) ? Promise.all(items) : value;
  }
  if (isPlainObject(value)) {
    const entries = Object.entries(value).map(([key, item]) => [key, settleDeep(item)]);
    if (!entries.some(([, item]) => isThenable(item))) {
      return value;
    }
    return Promise.all(entries.map(([, item]) => item)).then((settled) => Object.fromEntries(entries.map(([key], i7) => [key, settled[i7]])));
  }
  return value;
}

// node_modules/@a2ui/web_core/src/spec_versions.js
var SpecVersion;
(function(SpecVersion2) {
  SpecVersion2["V0_8"] = "v0.8";
  SpecVersion2["V0_9"] = "v0.9";
  SpecVersion2["V0_9_1"] = "v0.9.1";
  SpecVersion2["V1_0"] = "1.0";
})(SpecVersion || (SpecVersion = {}));

// node_modules/@a2ui/web_core/src/common/uax31.js
var UAX31_IDENTIFIER = new RegExp("^[\\p{XID_Start}_][\\p{XID_Continue}]*$", "u");
function isValidUax31Identifier(name, allowLeadingAt = false) {
  if (!name)
    return false;
  const testName = allowLeadingAt && name.startsWith("@") ? name.slice(1) : name;
  return UAX31_IDENTIFIER.test(testName);
}
function assertUax31Identifier(name, context, allowLeadingAt = false) {
  if (!isValidUax31Identifier(name, allowLeadingAt)) {
    throw new A2uiCatalogError(`Invalid UAX #31 ${context}`);
  }
}

// node_modules/@a2ui/web_core/src/common/metadata.js
function isPlainObject2(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
function validateExtensionsMap(extensions) {
  if (!isPlainObject2(extensions)) {
    throw new A2uiValidationError("Invalid metadata.extensions: expected a plain object.");
  }
  for (const key of Object.keys(extensions)) {
    if (!isValidUax31Identifier(key)) {
      throw new A2uiValidationError(`Invalid UAX #31 extension identifier: "${key}". Keys MUST be Unicode identifiers (UAX #31).`);
    }
  }
}
function validateMetadata(metadata) {
  if (metadata === void 0)
    return;
  if (!isPlainObject2(metadata)) {
    throw new A2uiValidationError("Invalid metadata: expected a plain object.");
  }
  for (const key of Object.keys(metadata)) {
    if (key !== "extensions") {
      throw new A2uiValidationError(`Unexpected property "${key}" in metadata; only "extensions" is allowed.`);
    }
  }
  if ("extensions" in metadata && metadata.extensions !== void 0) {
    validateExtensionsMap(metadata.extensions);
  }
}
function cloneAndFreezeValue(value) {
  if (Array.isArray(value)) {
    const clonedArray = value.map((item) => cloneAndFreezeValue(item));
    return Object.freeze(clonedArray);
  }
  if (isPlainObject2(value)) {
    const clonedObj = {};
    for (const [key, entry] of Object.entries(value)) {
      if (entry !== void 0) {
        Object.defineProperty(clonedObj, key, {
          value: cloneAndFreezeValue(entry),
          enumerable: true,
          configurable: true,
          writable: true
        });
      }
    }
    return Object.freeze(clonedObj);
  }
  return value;
}
function mergeActionMetadata(payloadMetadata, callSiteMetadata) {
  validateMetadata(payloadMetadata);
  validateMetadata(callSiteMetadata);
  if (payloadMetadata === void 0 && callSiteMetadata === void 0) {
    return void 0;
  }
  if (payloadMetadata === void 0) {
    return cloneMetadata(callSiteMetadata);
  }
  if (callSiteMetadata === void 0) {
    return cloneMetadata(payloadMetadata);
  }
  const mergedExtensions = {};
  if (isPlainObject2(payloadMetadata.extensions)) {
    for (const [key, val] of Object.entries(payloadMetadata.extensions)) {
      Object.defineProperty(mergedExtensions, key, {
        value: val,
        enumerable: true,
        configurable: true,
        writable: true
      });
    }
  }
  if (isPlainObject2(callSiteMetadata.extensions)) {
    for (const [key, val] of Object.entries(callSiteMetadata.extensions)) {
      Object.defineProperty(mergedExtensions, key, {
        value: val,
        enumerable: true,
        configurable: true,
        writable: true
      });
    }
  }
  const merged = Object.keys(mergedExtensions).length > 0 ? { extensions: mergedExtensions } : {};
  return cloneMetadata(merged);
}
function cloneMetadata(metadata) {
  if (metadata === void 0)
    return void 0;
  validateMetadata(metadata);
  if (!metadata.extensions || Object.keys(metadata.extensions).length === 0) {
    return void 0;
  }
  return cloneAndFreezeValue(metadata);
}
function arraysEqual(a4, b4) {
  if (a4.length !== b4.length)
    return false;
  for (let i7 = 0; i7 < a4.length; i7++) {
    if (!jsonValueEquals(a4[i7], b4[i7]))
      return false;
  }
  return true;
}
function objectsEqual(a4, b4) {
  const keysA = Object.keys(a4).filter((k2) => a4[k2] !== void 0);
  const keysB = Object.keys(b4).filter((k2) => b4[k2] !== void 0);
  if (keysA.length !== keysB.length)
    return false;
  for (const key of keysA) {
    if (!Object.prototype.hasOwnProperty.call(b4, key))
      return false;
    if (!jsonValueEquals(a4[key], b4[key]))
      return false;
  }
  return true;
}
function jsonValueEquals(a4, b4) {
  if (Object.is(a4, b4))
    return true;
  if (Array.isArray(a4) && Array.isArray(b4)) {
    return arraysEqual(a4, b4);
  }
  if (isPlainObject2(a4) && isPlainObject2(b4)) {
    return objectsEqual(a4, b4);
  }
  return false;
}
function metadataEquals(a4, b4) {
  return jsonValueEquals(a4, b4);
}

// node_modules/@a2ui/web_core/src/types/child-ref-helpers.js
function markChildRef(schema, ref) {
  schema._def.a2uiChildRef = ref;
  return schema;
}
function childRefKindOf(schema) {
  return schema?._def?.a2uiChildRef;
}

// node_modules/@a2ui/web_core/src/types/common-types.js
var ComponentIdSchema = markChildRef(external_exports.string().describe("REF:common_types.json#/$defs/ComponentId|The unique identifier for a component, used for both definitions and references within the same surface."), "component-id");
var DataBindingSchema = external_exports.object({
  "path": external_exports.string().describe("A JSON Pointer path to a value in the data model.").optional(),
  "@path": external_exports.string().describe("A JSON Pointer path to a value in the data model.").optional()
}).refine((data) => data["@path"] !== void 0 || data.path !== void 0, {
  message: "Either '@path' or 'path' must be provided."
}).describe("REF:common_types.json#/$defs/DataBinding");
var FunctionCallSchema = external_exports.object({
  "call": external_exports.string().describe("The name of the function to call.").optional(),
  "@call": external_exports.string().describe("The name of the function to call.").optional(),
  "args": external_exports.record(external_exports.string(), external_exports.unknown()).describe("Arguments passed to the function.").optional(),
  "returnType": external_exports.enum(["string", "number", "boolean", "array", "object", "any", "void"]).describe("The expected return type of the function call.").optional(),
  "catalogId": external_exports.string().describe("The catalog ID for this function, overriding any surface-level default catalogId.").optional()
}).strict().refine((data) => data["@call"] !== void 0 || data.call !== void 0, {
  message: "Either '@call' or 'call' must be provided."
}).describe("REF:common_types.json#/$defs/FunctionCall|Invokes a named function, combining common function properties with the catalog function definition.");
var DynamicStringSchema = external_exports.union([external_exports.string(), DataBindingSchema, FunctionCallSchema]).describe("REF:common_types.json#/$defs/DynamicString|Represents a string");
var DynamicBooleanSchema = external_exports.union([external_exports.boolean(), DataBindingSchema, FunctionCallSchema]).describe("REF:common_types.json#/$defs/DynamicBoolean|A boolean value that can be a literal, a path, or a function call returning a boolean or a validation result object (coerced to its \\'valid\\' boolean).");
var AccessibilityAttributesSchema = external_exports.object({
  "label": DynamicStringSchema.optional(),
  "description": DynamicStringSchema.optional(),
  "live": external_exports.enum(["off", "polite", "assertive"]).default("off").describe("Controls screen reader announcements for dynamic updates (WAI-ARIA aria-live). 'polite' waits for user pause; 'assertive' interrupts immediately for alerts.").optional(),
  "hidden": DynamicBooleanSchema.optional()
}).describe("REF:common_types.json#/$defs/AccessibilityAttributes|Attributes to enhance accessibility when using assistive technologies like screen readers or model understanding.");
var ExtensionsSchema = external_exports.record(external_exports.string(), external_exports.unknown()).superRefine((value, ctx) => {
  for (const key in value) {
    if (!isValidUax31Identifier(key)) {
      ctx.addIssue({
        path: [key],
        code: external_exports.ZodIssueCode.custom,
        message: `Invalid extension key "${key}": Keys MUST be Unicode identifiers (UAX #31).`
      });
    }
  }
}).describe("REF:#/$defs/Extensions|Optional extension metadata. Keys MUST be Unicode identifiers (UAX #31). Keys starting with 'a2ui_' are reserved for official extensions.");
var ComponentCommonSchema = external_exports.object({
  "id": ComponentIdSchema,
  "accessibility": AccessibilityAttributesSchema.optional(),
  "catalogId": external_exports.string().describe("The catalog ID for this component, overriding any surface-level default catalogId.").optional(),
  "metadata": external_exports.object({
    "extensions": ExtensionsSchema.optional()
  }).strict().describe("Optional component-level metadata for vendor extensions.").optional()
}).describe("REF:common_types.json#/$defs/ComponentCommon");
var ChildListSchema = markChildRef(external_exports.union([
  external_exports.array(ComponentIdSchema).describe("A static list of child component IDs."),
  external_exports.object({
    "componentId": ComponentIdSchema,
    "path": external_exports.string().describe("The path to the list of component property objects in the data model.")
  }).describe("A template for generating a dynamic list of children.")
]).describe("REF:common_types.json#/$defs/ChildList"), "child-list");
var DynamicValueSchema = external_exports.union([
  external_exports.string(),
  external_exports.number(),
  external_exports.boolean(),
  external_exports.array(external_exports.unknown()),
  DataBindingSchema,
  FunctionCallSchema,
  external_exports.record(external_exports.string(), external_exports.unknown()).refine((obj) => !obj || !("@path" in obj) && !("path" in obj) && !("@call" in obj) && !("call" in obj))
]).describe("REF:common_types.json#/$defs/DynamicValue|A value that can be a literal, a path, or a function call returning any type.");
var DynamicNumberSchema = external_exports.union([external_exports.number(), DataBindingSchema, FunctionCallSchema]).describe("REF:common_types.json#/$defs/DynamicNumber|Represents a value that can be either a literal number, a path to a number in the data model, or a function call returning a number.");
var DynamicStringListSchema = external_exports.union([external_exports.array(external_exports.string()), DataBindingSchema, FunctionCallSchema]).describe("REF:common_types.json#/$defs/DynamicStringList|Represents a value that can be either a literal array of strings, a path to a string array in the data model, or a function call returning a string array.");
var CheckRuleSchema = external_exports.object({
  "condition": external_exports.union([DynamicBooleanSchema, DataBindingSchema, FunctionCallSchema]).describe("Path or function call evaluating to a structured validation result object or a boolean."),
  "message": external_exports.string().describe("Optional fallback error message.").optional()
}).describe("REF:common_types.json#/$defs/CheckRule|A single validation check rule applied to an input component. The condition function or path evaluates to a structured validation result object or a boolean (which is coerced to a validation result object).");
var CheckableSchema = external_exports.object({
  "checks": external_exports.array(CheckRuleSchema).describe("A list of validation check rules to perform on the component.").optional()
}).describe("REF:common_types.json#/$defs/Checkable|Properties for components that support renderer-side checks.");
var ActionSchema = external_exports.union([
  external_exports.object({
    "event": external_exports.object({
      "name": external_exports.string().describe("The name of the action to be dispatched to the server."),
      "context": external_exports.record(external_exports.string(), external_exports.unknown()).describe("A JSON object containing the key-value pairs for the action context. Values can be literals or paths. Use literal values unless the value must be dynamically bound to the data model. Do NOT use paths for static IDs.").optional()
    }).strict().describe("The event to dispatch to the server.")
  }).strict().describe("Triggers a server-side event."),
  external_exports.object({
    "functionCall": FunctionCallSchema
  }).strict().describe("Executes a local client-side function."),
  external_exports.object({
    "event": external_exports.object({
      "name": external_exports.string().describe("The name of the action to be dispatched to the agent."),
      "userMessage": DynamicStringSchema.optional(),
      "context": external_exports.record(external_exports.string(), external_exports.unknown()).describe("A JSON object containing the key-value pairs for the action context. Values can be literals or paths. Use literal values unless the value must be dynamically bound to the data model. Do NOT use paths for static IDs.").optional()
    }).strict().describe("The event to dispatch to the agent.")
  }).strict().describe("Triggers an agent-side event."),
  external_exports.object({
    "functionCall": FunctionCallSchema
  }).strict().describe("Executes a renderer or agent-side function.")
]).describe("REF:common_types.json#/$defs/Action|Defines an interaction handler that can either trigger an agent-side event or execute a local renderer-side function.");
var CallIdSchema = external_exports.string().describe("REF:common_types.json#/$defs/CallId|The unique identifier for a function call.");
var ChildSchema = ComponentIdSchema;
var FunctionCommonSchema = external_exports.object({
  "call": external_exports.string().describe("The name of the function to call.").optional(),
  "@call": external_exports.string().describe("The name of the function to call.").optional(),
  "catalogId": external_exports.string().describe("The catalog ID for this function, overriding any surface-level default catalogId.").optional()
}).refine((data) => data["@call"] !== void 0 || data.call !== void 0, {
  message: "Either '@call' or 'call' must be provided."
}).describe("REF:common_types.json#/$defs/FunctionCommon|Baseline envelope properties common to all function calls. Function-specific argument schemas ('args') are defined individually by each function in the active catalog.");
var IndexSystemFunctionSchema = external_exports.object({
  "call": external_exports.literal("@index").optional(),
  "@call": external_exports.literal("@index").optional(),
  "args": external_exports.object({
    "offset": DynamicNumberSchema.optional()
  }).optional()
}).refine((data) => data["@call"] !== void 0 || data.call !== void 0, {
  message: "Either '@call' or 'call' must be '@index'."
}).describe("REF:common_types.json#/$defs/IndexSystemFunction|Returns the 0-based index of the current item when rendering a dynamic list from a template. This function MUST ONLY be available when evaluating template items within a list context.");
var SurfaceSchema = external_exports.object({
  "component": external_exports.literal("Surface").optional(),
  "child": external_exports.literal("root").optional()
}).strict().describe("REF:common_types.json#/$defs/Surface|The reserved canonical container component representing an A2UI surface. The Surface component is immutable and always has \\'child\\': \\'root\\'.");
var FunctionResponseSchema = external_exports.object({
  "functionCallId": CallIdSchema,
  "value": external_exports.unknown().describe("The return value of the function.").optional(),
  "error": external_exports.object({
    "code": external_exports.string(),
    "message": external_exports.string()
  }).strict().describe("An error object indicating failure of the function execution.").optional()
}).strict().describe("REF:common_types.json#/$defs/FunctionResponse|The return response matching a callAgentFunction or callRendererFunction invocation.");
var CommonSchemas = {
  ComponentId: ComponentIdSchema,
  DataBinding: DataBindingSchema,
  FunctionCall: FunctionCallSchema,
  DynamicString: DynamicStringSchema,
  DynamicBoolean: DynamicBooleanSchema,
  AccessibilityAttributes: AccessibilityAttributesSchema,
  Extensions: ExtensionsSchema,
  ComponentCommon: ComponentCommonSchema,
  ChildList: ChildListSchema,
  DynamicValue: DynamicValueSchema,
  DynamicNumber: DynamicNumberSchema,
  DynamicStringList: DynamicStringListSchema,
  CheckRule: CheckRuleSchema,
  Checkable: CheckableSchema,
  Action: ActionSchema,
  CallId: CallIdSchema,
  Child: ChildSchema,
  FunctionCommon: FunctionCommonSchema,
  IndexSystemFunction: IndexSystemFunctionSchema,
  Surface: SurfaceSchema,
  FunctionResponse: FunctionResponseSchema
};

// node_modules/@a2ui/web_core/src/catalog/exclusive_union.js
var exclusiveGuards = /* @__PURE__ */ new WeakSet();
function exclusiveUnion(branches) {
  const guard = external_exports.any().superRefine((val, ctx) => {
    let matches = 0;
    for (const b4 of branches) {
      if (b4.safeParse(val).success && ++matches > 1) {
        ctx.addIssue({
          code: external_exports.ZodIssueCode.custom,
          message: "Value matched more than one schema in oneOf"
        });
        return;
      }
    }
  });
  exclusiveGuards.add(guard._def);
  return guard.pipe(external_exports.union(branches));
}
var exclusiveUnionOverride = (def, refs) => {
  const pipe = def;
  if (!pipe.in || !pipe.out || !exclusiveGuards.has(pipe.in._def))
    return ignoreOverride;
  const options = pipe.out._def.options;
  const oneOf = options.map((option, index) => parseDef(option._def, { ...refs, currentPath: [...refs.currentPath, "oneOf", `${index}`] })).filter((branch) => branch !== void 0);
  return def.description === void 0 ? { oneOf } : { oneOf, description: def.description };
};

// node_modules/@a2ui/web_core/src/catalog/json_entries.js
var definitions = /* @__PURE__ */ new WeakMap();
function loadedDefinition(entry) {
  return definitions.get(entry);
}
function defineViews(target, views) {
  for (const [key, compute] of Object.entries(views)) {
    let computed2 = false;
    let value;
    Object.defineProperty(target, key, {
      enumerable: true,
      get() {
        if (!computed2) {
          value = compute();
          computed2 = true;
        }
        return value;
      }
    });
  }
}
function isObject(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
function stringList(value) {
  return Array.isArray(value) ? value.filter((item) => typeof item === "string") : void 0;
}
var JsonComponentApi = class {
  /**
   * @param name The component's name.
   * @param jsonSchema The component's definition, deeply frozen.
   * @param deriveSchema Derives the component's Zod schema from its definition.
   */
  constructor(name, jsonSchema, deriveSchema) {
    this.name = name;
    definitions.set(this, jsonSchema);
    defineViews(this, {
      schema: () => deriveSchema(jsonSchema),
      allowedParents: () => stringList(jsonSchema["allowedParents"]),
      allowedChildren: () => stringList(jsonSchema["allowedChildren"]),
      metadata: () => isObject(jsonSchema["metadata"]) ? jsonSchema["metadata"] : void 0
    });
    Object.freeze(this);
  }
  /** The component's JSON Schema definition, as its catalog document writes it. */
  get jsonSchema() {
    return definitions.get(this);
  }
};
var JsonFunctionApi = class {
  /**
   * @param name The function's name.
   * @param jsonSchema The function's definition, deeply frozen.
   * @param deriveSchema Derives the Zod schema of the function's arguments
   *     from its definition.
   */
  constructor(name, jsonSchema, deriveSchema) {
    this.name = name;
    definitions.set(this, jsonSchema);
    const props = isObject(jsonSchema["properties"]) ? jsonSchema["properties"] : void 0;
    const constant = (key) => {
      const property = props?.[key];
      return isObject(property) ? property["const"] : void 0;
    };
    defineViews(this, {
      schema: () => deriveSchema(jsonSchema),
      returnType: () => {
        const declared = jsonSchema["returnType"] ?? constant("returnType");
        return typeof declared === "string" ? declared : "any";
      },
      description: () => typeof jsonSchema["description"] === "string" ? jsonSchema["description"] : void 0,
      allowedCallers: () => jsonSchema["allowedCallers"] ?? constant("allowedCallers"),
      requiresUserActivation: () => jsonSchema["requiresUserActivation"] ?? constant("requiresUserActivation")
    });
    Object.freeze(this);
  }
  /** The function's JSON Schema definition, as its catalog document writes it. */
  get jsonSchema() {
    return definitions.get(this);
  }
};

// node_modules/@a2ui/web_core/src/catalog/spec_annotations.js
var annotations = /* @__PURE__ */ new WeakMap();
function cloneWithDefAnnotation(schema, nextAnnotation, description = schema.description) {
  const copy = external_exports.ZodType.prototype.describe.call(schema, description);
  annotations.set(copy, nextAnnotation);
  copy.describe = (nextDescription) => cloneWithDefAnnotation(copy, annotations.get(copy) ?? nextAnnotation, nextDescription);
  return copy;
}
function spec(schema, annotation) {
  const copy = external_exports.ZodType.prototype.describe.call(schema, schema.description);
  annotations.set(copy, { ...annotations.get(schema), ...annotation });
  return copy;
}
function specDef(name, schema, annotation = {}) {
  const copy = cloneWithDefAnnotation(schema, {
    ...annotations.get(schema),
    ...annotation,
    defName: name
  });
  annotations.set(copy, {
    ...annotations.get(copy),
    defName: name,
    defModel: copy
  });
  return copy;
}
function getSpecAnnotation(schema) {
  return typeof schema === "object" && schema !== null ? annotations.get(schema) : void 0;
}

// node_modules/@a2ui/web_core/src/v0_8/standard_defs.js
var V08_CHILD_REF_OPTIONS = {
  childRefNames: /* @__PURE__ */ new Set(["ComponentId", "Child", "ChildComponentId"]),
  childListRefNames: /* @__PURE__ */ new Set(["ChildList", "TemplateChildList"]),
  childPropertyNames: /* @__PURE__ */ new Set(["child", "entryPointChild", "contentChild"])
};
var V08_STANDARD_DEFS = {
  "ComponentId": {
    "description": "The unique identifier for a component, used for both definitions and references within the same surface.",
    "type": "string"
  },
  "TextItem": {
    "additionalProperties": false,
    "description": "The value of the text field. This can be a literal string or a reference to a value in the data model ('path', e.g. '/user/name').",
    "properties": {
      "literalString": {
        "type": "string"
      },
      "path": {
        "type": "string"
      }
    },
    "type": "object"
  },
  "UrlItem": {
    "additionalProperties": false,
    "description": "The URL of the audio to be played. This can be a literal string ('literal') or a reference to a value in the data model ('path', e.g. '/song/url').",
    "properties": {
      "literalString": {
        "type": "string"
      },
      "path": {
        "type": "string"
      }
    },
    "type": "object"
  },
  "AltTextItem": {
    "additionalProperties": false,
    "description": "The alt text for the image. This can be a literal string ('literal') or a reference to a value in the data model ('path', e.g. '/thumbnail/altText').",
    "properties": {
      "literalString": {
        "type": "string"
      },
      "path": {
        "type": "string"
      }
    },
    "type": "object"
  },
  "NameItem": {
    "additionalProperties": false,
    "description": "The name of the icon to display. This can be a literal string or a reference to a value in the data model ('path', e.g. '/form/submit').",
    "properties": {
      "literalString": {
        "enum": [
          "accountCircle",
          "add",
          "arrowBack",
          "arrowForward",
          "attachFile",
          "calendarToday",
          "call",
          "camera",
          "check",
          "close",
          "delete",
          "download",
          "edit",
          "event",
          "error",
          "favorite",
          "favoriteOff",
          "folder",
          "help",
          "home",
          "info",
          "locationOn",
          "lock",
          "lockOpen",
          "mail",
          "menu",
          "moreVert",
          "moreHoriz",
          "notificationsOff",
          "notifications",
          "payment",
          "person",
          "phone",
          "photo",
          "print",
          "refresh",
          "search",
          "send",
          "settings",
          "share",
          "shoppingCart",
          "star",
          "starHalf",
          "starOff",
          "upload",
          "visibility",
          "visibilityOff",
          "warning"
        ],
        "type": "string"
      },
      "path": {
        "type": "string"
      }
    },
    "type": "object"
  },
  "DescriptionItem": {
    "additionalProperties": false,
    "description": "A description of the audio, such as a title or summary. This can be a literal string or a reference to a value in the data model ('path', e.g. '/song/title').",
    "properties": {
      "literalString": {
        "type": "string"
      },
      "path": {
        "type": "string"
      }
    },
    "type": "object"
  },
  "ChildrenItem": {
    "additionalProperties": false,
    "description": "Defines the children. Use 'explicitList' for a fixed set of children, or 'template' to generate children from a data list.",
    "properties": {
      "explicitList": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "template": {
        "$ref": "#/$defs/TemplateItem",
        "description": "A template for generating a dynamic list of children from a data model list. `componentId` is the component to use as a template, and `dataBinding` is the path to the map of components in the data model. Values in the map will define the list of children."
      }
    },
    "type": "object"
  },
  "TemplateItem": {
    "additionalProperties": false,
    "description": "A template for generating a dynamic list of children from a data model list. `componentId` is the component to use as a template, and `dataBinding` is the path to the map of components in the data model. Values in the map will define the list of children.",
    "properties": {
      "componentId": {
        "type": "string"
      },
      "dataBinding": {
        "type": "string"
      }
    },
    "required": ["componentId", "dataBinding"],
    "type": "object"
  },
  "TabItemItem": {
    "additionalProperties": false,
    "properties": {
      "child": {
        "type": "string"
      },
      "title": {
        "$ref": "#/$defs/TitleItem"
      }
    },
    "required": ["title", "child"],
    "type": "object"
  },
  "TitleItem": {
    "additionalProperties": false,
    "description": "The tab title. Defines the value as either a literal value or a path to data model value (e.g. '/options/title').",
    "properties": {
      "literalString": {
        "type": "string"
      },
      "path": {
        "type": "string"
      }
    },
    "type": "object"
  },
  "ActionItem": {
    "additionalProperties": false,
    "description": "The client-side action to be dispatched when the button is clicked. It includes the action's name and an optional context payload.",
    "properties": {
      "context": {
        "items": {
          "$ref": "#/$defs/ContextItem"
        },
        "type": "array"
      },
      "name": {
        "type": "string"
      }
    },
    "required": ["name"],
    "type": "object"
  },
  "ContextItem": {
    "additionalProperties": false,
    "properties": {
      "key": {
        "type": "string"
      },
      "value": {
        "$ref": "#/$defs/ValueItem",
        "description": "Defines the value to be included in the context as either a literal value or a path to a data model value (e.g. '/user/name')."
      }
    },
    "required": ["key", "value"],
    "type": "object"
  },
  "ValueItem": {
    "additionalProperties": false,
    "description": "The current value of the slider. This can be a literal number ('literalNumber') or a reference to a value in the data model ('path', e.g. '/restaurant/cost').",
    "properties": {
      "literalNumber": {
        "type": "number"
      },
      "path": {
        "type": "string"
      }
    },
    "type": "object"
  },
  "LabelItem": {
    "additionalProperties": false,
    "description": "The label for the slider. This can be a literal string or a reference to a value in the data model ('path').",
    "properties": {
      "literalString": {
        "type": "string"
      },
      "path": {
        "type": "string"
      }
    },
    "type": "object"
  },
  "OptionItem": {
    "additionalProperties": false,
    "properties": {
      "label": {
        "$ref": "#/$defs/LabelItem",
        "description": "The text to display for this option. This can be a literal string or a reference to a value in the data model (e.g. '/option/label')."
      },
      "value": {
        "description": "The value to be associated with this option when selected.",
        "type": "string"
      }
    },
    "required": ["label", "value"],
    "type": "object"
  },
  "SelectionItem": {
    "additionalProperties": false,
    "description": "The currently selected values for the component. This can be a literal array of strings or a path to an array in the data model('path', e.g. '/hotel/options').",
    "properties": {
      "literalArray": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "path": {
        "type": "string"
      }
    },
    "type": "object"
  }
};

// node_modules/@a2ui/web_core/src/v0_9/standard_defs.js
var V09_CHILD_REF_OPTIONS = {
  childRefNames: /* @__PURE__ */ new Set(["ComponentId", "Child", "ChildComponentId"]),
  childListRefNames: /* @__PURE__ */ new Set(["ChildList", "TemplateChildList", "ChildrenItem", "TemplateItem"])
};
var V09_STANDARD_DEFS = {
  "ComponentId": {
    "type": "string",
    "description": "The unique identifier for a component, used for both definitions and references within the same surface."
  },
  "AccessibilityAttributes": {
    "type": "object",
    "description": "Attributes to enhance accessibility when using assistive technologies like screen readers.",
    "properties": {
      "label": {
        "$ref": "#/$defs/DynamicString",
        "description": "A short string, typically 1 to 3 words, used by assistive technologies to convey the purpose or intent of an element. For example, an input field might have an accessible label of 'User ID' or a button might be labeled 'Submit'."
      },
      "description": {
        "$ref": "#/$defs/DynamicString",
        "description": "Additional information provided by assistive technologies about an element such as instructions, format requirements, or result of an action. For example, a mute button might have a label of 'Mute' and a description of 'Silences notifications about this conversation'."
      }
    }
  },
  "ComponentCommon": {
    "type": "object",
    "properties": {
      "id": {
        "$ref": "#/$defs/ComponentId"
      },
      "accessibility": {
        "$ref": "#/$defs/AccessibilityAttributes"
      }
    },
    "required": ["id"]
  },
  "ChildList": {
    "oneOf": [
      {
        "type": "array",
        "description": "A static list of child component IDs.",
        "items": {
          "$ref": "#/$defs/ComponentId"
        }
      },
      {
        "type": "object",
        "description": "A template for generating a dynamic list of children from a data model list. The `componentId` is the component to use as a template.",
        "properties": {
          "componentId": {
            "$ref": "#/$defs/ComponentId"
          },
          "path": {
            "type": "string",
            "description": "The path to the list of component property objects in the data model."
          }
        },
        "required": ["componentId", "path"],
        "additionalProperties": false
      }
    ]
  },
  "DataBinding": {
    "type": "object",
    "properties": {
      "path": {
        "type": "string",
        "description": "A JSON Pointer path to a value in the data model."
      }
    },
    "required": ["path"],
    "additionalProperties": false
  },
  "DynamicValue": {
    "description": "A value that can be a literal, a path, or a function call returning any type.",
    "oneOf": [
      {
        "type": "string"
      },
      {
        "type": "number"
      },
      {
        "type": "boolean"
      },
      {
        "type": "array"
      },
      {
        "$ref": "#/$defs/DataBinding"
      },
      {
        "$ref": "#/$defs/FunctionCall"
      }
    ]
  },
  "DynamicString": {
    "description": "Represents a string",
    "oneOf": [
      {
        "type": "string"
      },
      {
        "$ref": "#/$defs/DataBinding"
      },
      {
        "allOf": [
          {
            "$ref": "#/$defs/FunctionCall"
          },
          {
            "properties": {
              "returnType": {
                "const": "string"
              }
            }
          }
        ]
      }
    ]
  },
  "DynamicNumber": {
    "description": "Represents a value that can be either a literal number, a path to a number in the data model, or a function call returning a number.",
    "oneOf": [
      {
        "type": "number"
      },
      {
        "$ref": "#/$defs/DataBinding"
      },
      {
        "allOf": [
          {
            "$ref": "#/$defs/FunctionCall"
          },
          {
            "properties": {
              "returnType": {
                "const": "number"
              }
            }
          }
        ]
      }
    ]
  },
  "DynamicBoolean": {
    "description": "A boolean value that can be a literal, a path, or a function call returning a boolean.",
    "oneOf": [
      {
        "type": "boolean"
      },
      {
        "$ref": "#/$defs/DataBinding"
      },
      {
        "allOf": [
          {
            "$ref": "#/$defs/FunctionCall"
          },
          {
            "properties": {
              "returnType": {
                "const": "boolean"
              }
            }
          }
        ]
      }
    ]
  },
  "DynamicStringList": {
    "description": "Represents a value that can be either a literal array of strings, a path to a string array in the data model, or a function call returning a string array.",
    "oneOf": [
      {
        "type": "array",
        "items": {
          "type": "string"
        }
      },
      {
        "$ref": "#/$defs/DataBinding"
      },
      {
        "allOf": [
          {
            "$ref": "#/$defs/FunctionCall"
          },
          {
            "properties": {
              "returnType": {
                "const": "array"
              }
            }
          }
        ]
      }
    ]
  },
  "FunctionCall": {
    "type": "object",
    "description": "Invokes a named function on the client.",
    "properties": {
      "call": {
        "type": "string",
        "description": "The name of the function to call."
      },
      "args": {
        "type": "object",
        "description": "Arguments passed to the function.",
        "additionalProperties": {
          "anyOf": [
            {
              "$ref": "#/$defs/DynamicValue"
            },
            {
              "type": "object",
              "description": "A literal object argument (e.g. configuration)."
            }
          ]
        }
      },
      "returnType": {
        "type": "string",
        "description": "The expected return type of the function call.",
        "enum": ["string", "number", "boolean", "array", "object", "any", "void"],
        "default": "boolean"
      }
    },
    "required": ["call"],
    "oneOf": [
      {
        "$ref": "catalog.json#/$defs/anyFunction"
      }
    ]
  },
  "CheckRule": {
    "type": "object",
    "description": "A single validation rule applied to an input component.",
    "properties": {
      "condition": {
        "$ref": "#/$defs/DynamicBoolean"
      },
      "message": {
        "type": "string",
        "description": "The error message to display if the check fails."
      }
    },
    "required": ["condition", "message"],
    "additionalProperties": false
  },
  "Checkable": {
    "type": "object",
    "description": "Properties for components that support client-side checks.",
    "properties": {
      "checks": {
        "type": "array",
        "description": "A list of checks to perform. These are function calls that must return a boolean indicating validity.",
        "items": {
          "$ref": "#/$defs/CheckRule"
        }
      }
    }
  },
  "Action": {
    "description": "Defines an interaction handler that can either trigger a server-side event or execute a local client-side function.",
    "oneOf": [
      {
        "type": "object",
        "description": "Triggers a server-side event.",
        "properties": {
          "event": {
            "type": "object",
            "description": "The event to dispatch to the server.",
            "properties": {
              "name": {
                "type": "string",
                "description": "The name of the action to be dispatched to the server."
              },
              "context": {
                "type": "object",
                "description": "A JSON object containing the key-value pairs for the action context. Values can be literals or paths. Use literal values unless the value must be dynamically bound to the data model. Do NOT use paths for static IDs.",
                "additionalProperties": {
                  "$ref": "#/$defs/DynamicValue"
                }
              }
            },
            "required": ["name"],
            "additionalProperties": false
          }
        },
        "required": ["event"],
        "additionalProperties": false
      },
      {
        "type": "object",
        "description": "Executes a local client-side function.",
        "properties": {
          "functionCall": {
            "$ref": "#/$defs/FunctionCall"
          }
        },
        "required": ["functionCall"],
        "additionalProperties": false
      }
    ]
  }
};

// node_modules/@a2ui/web_core/src/v1_0/standard_defs.js
var V10_CHILD_REF_OPTIONS = {
  childRefNames: /* @__PURE__ */ new Set(["ComponentId", "Child", "ChildComponentId"]),
  childListRefNames: /* @__PURE__ */ new Set(["ChildList", "TemplateChildList", "ChildrenItem", "TemplateItem"])
};
var V10_STANDARD_DEFS = {
  "ComponentId": {
    "type": "string",
    "description": "The unique identifier for a component, used for both definitions and references within the same surface."
  },
  "CallId": {
    "type": "string",
    "description": "The unique identifier for a function call."
  },
  "AccessibilityAttributes": {
    "type": "object",
    "description": "Attributes to enhance accessibility when using assistive technologies like screen readers or model understanding.",
    "properties": {
      "label": {
        "$ref": "#/$defs/DynamicString",
        "description": "A short string, typically 1 to 3 words, used by assistive technologies to convey the purpose or intent of an element. For example, an input field might have an accessible label of 'User ID' or a button might be labeled 'Submit'."
      },
      "description": {
        "$ref": "#/$defs/DynamicString",
        "description": "Additional information provided by assistive technologies about an element such as instructions, format requirements, or result of an action. For example, a mute button might have a label of 'Mute' and a description of 'Silences notifications about this conversation'."
      },
      "live": {
        "type": "string",
        "description": "Controls screen reader announcements for dynamic updates (WAI-ARIA aria-live). 'polite' waits for user pause; 'assertive' interrupts immediately for alerts.",
        "enum": ["off", "polite", "assertive"],
        "default": "off"
      },
      "hidden": {
        "$ref": "#/$defs/DynamicBoolean",
        "description": "Hides the element and its children from assistive technologies when true. Default is false."
      }
    },
    "additionalProperties": false
  },
  "Extensions": {
    "type": "object",
    "description": "Optional extension metadata. Keys MUST be Unicode identifiers (UAX #31). Keys starting with 'a2ui_' are reserved for official extensions.",
    "patternProperties": {
      "^[\\p{XID_Start}_][\\p{XID_Continue}]*$": {}
    },
    "additionalProperties": false
  },
  "ComponentCommon": {
    "type": "object",
    "properties": {
      "id": {
        "$ref": "#/$defs/ComponentId"
      },
      "catalogId": {
        "type": "string",
        "description": "The catalog ID for this component, overriding any surface-level default catalogId."
      },
      "accessibility": {
        "$ref": "#/$defs/AccessibilityAttributes"
      },
      "metadata": {
        "type": "object",
        "description": "Optional component-level metadata for vendor extensions.",
        "properties": {
          "extensions": {
            "$ref": "#/$defs/Extensions"
          }
        },
        "additionalProperties": false
      }
    },
    "required": ["id"]
  },
  "Child": {
    "$ref": "#/$defs/ComponentId",
    "description": "A reference to a single child component ID."
  },
  "ChildList": {
    "oneOf": [
      {
        "type": "array",
        "description": "A static list of child component IDs.",
        "items": {
          "$ref": "#/$defs/ComponentId"
        }
      },
      {
        "type": "object",
        "description": "A template for generating a dynamic list of children from a data model list. The `componentId` is the component to use as a template.",
        "properties": {
          "componentId": {
            "$ref": "#/$defs/ComponentId"
          },
          "path": {
            "type": "string",
            "description": "The path to the list of component property objects in the data model."
          }
        },
        "required": ["componentId", "path"],
        "additionalProperties": false
      }
    ]
  },
  "DataBinding": {
    "type": "object",
    "properties": {
      "@path": {
        "type": "string",
        "description": "A JSON Pointer path to a value in the data model."
      }
    },
    "required": ["@path"],
    "additionalProperties": false
  },
  "DynamicValue": {
    "description": "A value that can be a literal, a path, or a function call returning any type.",
    "oneOf": [
      {
        "type": "string"
      },
      {
        "type": "number"
      },
      {
        "type": "boolean"
      },
      {
        "type": "array"
      },
      {
        "type": "object",
        "propertyNames": {
          "not": {
            "pattern": "^@([^@]|$)"
          }
        },
        "not": {
          "anyOf": [
            {
              "required": ["@path"]
            },
            {
              "required": ["@call"]
            }
          ]
        }
      },
      {
        "$ref": "#/$defs/DataBinding"
      },
      {
        "$ref": "#/$defs/FunctionCall"
      }
    ]
  },
  "DynamicString": {
    "description": "Represents a string",
    "oneOf": [
      {
        "type": "string"
      },
      {
        "$ref": "#/$defs/DataBinding"
      },
      {
        "$ref": "#/$defs/FunctionCall"
      }
    ]
  },
  "DynamicNumber": {
    "description": "Represents a value that can be either a literal number, a path to a number in the data model, or a function call returning a number.",
    "oneOf": [
      {
        "type": "number"
      },
      {
        "$ref": "#/$defs/DataBinding"
      },
      {
        "$ref": "#/$defs/FunctionCall"
      }
    ]
  },
  "DynamicBoolean": {
    "description": "A boolean value that can be a literal, a path, or a function call returning a boolean or a validation result object (coerced to its 'valid' boolean).",
    "oneOf": [
      {
        "type": "boolean"
      },
      {
        "$ref": "#/$defs/DataBinding"
      },
      {
        "$ref": "#/$defs/FunctionCall"
      }
    ]
  },
  "DynamicStringList": {
    "description": "Represents a value that can be either a literal array of strings, a path to a string array in the data model, or a function call returning a string array.",
    "oneOf": [
      {
        "type": "array",
        "items": {
          "type": "string"
        }
      },
      {
        "$ref": "#/$defs/DataBinding"
      },
      {
        "$ref": "#/$defs/FunctionCall"
      }
    ]
  },
  "FunctionCommon": {
    "type": "object",
    "description": "Baseline envelope properties common to all function calls. Function-specific argument schemas ('args') are defined individually by each function in the active catalog.",
    "properties": {
      "@call": {
        "type": "string",
        "description": "The name of the function to call."
      },
      "catalogId": {
        "type": "string",
        "description": "The catalog ID for this function, overriding any surface-level default catalogId."
      }
    },
    "required": ["@call"]
  },
  "IndexSystemFunction": {
    "type": "object",
    "description": "Returns the 0-based index of the current item when rendering a dynamic list from a template. This function MUST ONLY be available when evaluating template items within a list context.",
    "properties": {
      "@call": {
        "const": "@index"
      },
      "args": {
        "type": "object",
        "properties": {
          "offset": {
            "$ref": "#/$defs/DynamicNumber",
            "description": "Optional. An offset to add to the 0-based index (e.g., 1 for 1-based indexing). Defaults to 0.",
            "default": 0
          }
        },
        "unevaluatedProperties": false
      }
    },
    "required": ["@call"],
    "unevaluatedProperties": false
  },
  "FunctionCall": {
    "type": "object",
    "description": "Invokes a named function, combining common function properties with the catalog function definition.",
    "allOf": [
      {
        "$ref": "#/$defs/FunctionCommon"
      },
      {
        "oneOf": [
          {
            "$ref": "catalog.json#/$defs/anyFunction"
          },
          {
            "$ref": "#/$defs/IndexSystemFunction"
          }
        ]
      }
    ],
    "unevaluatedProperties": false
  },
  "CheckRule": {
    "type": "object",
    "description": "A single validation check rule applied to an input component. The condition function or path evaluates to a structured validation result object or a boolean (which is coerced to a validation result object).",
    "properties": {
      "condition": {
        "description": "Path or function call evaluating to a structured validation result object or a boolean.",
        "oneOf": [
          {
            "$ref": "#/$defs/DataBinding"
          },
          {
            "$ref": "#/$defs/FunctionCall"
          }
        ]
      },
      "message": {
        "type": "string",
        "description": "Optional fallback error message."
      }
    },
    "required": ["condition"],
    "additionalProperties": false
  },
  "Checkable": {
    "type": "object",
    "description": "Properties for components that support renderer-side checks.",
    "properties": {
      "checks": {
        "type": "array",
        "description": "A list of validation check rules to perform on the component.",
        "items": {
          "$ref": "#/$defs/CheckRule"
        }
      }
    }
  },
  "Action": {
    "description": "Defines an interaction handler that can either trigger an agent-side event or execute a local renderer-side function.",
    "oneOf": [
      {
        "type": "object",
        "description": "Triggers an agent-side event.",
        "properties": {
          "event": {
            "type": "object",
            "description": "The event to dispatch to the agent.",
            "properties": {
              "name": {
                "type": "string",
                "description": "The name of the action to be dispatched to the agent."
              },
              "userMessage": {
                "$ref": "#/$defs/DynamicString",
                "description": "An optional human-readable message describing the action performed by the user, resolved by the renderer and included in the outbound action message."
              },
              "context": {
                "type": "object",
                "description": "A JSON object containing the key-value pairs for the action context. Values can be literals or paths. Use literal values unless the value must be dynamically bound to the data model. Do NOT use paths for static IDs.",
                "additionalProperties": {
                  "$ref": "#/$defs/DynamicValue"
                }
              }
            },
            "required": ["name"],
            "additionalProperties": false
          }
        },
        "required": ["event"],
        "additionalProperties": false
      },
      {
        "type": "object",
        "description": "Executes a renderer or agent-side function.",
        "properties": {
          "functionCall": {
            "$ref": "#/$defs/FunctionCall"
          }
        },
        "required": ["functionCall"],
        "additionalProperties": false
      }
    ]
  },
  "Surface": {
    "title": "Surface Container Component",
    "description": "The reserved canonical container component representing an A2UI surface. The Surface component is immutable and always has 'child': 'root'.",
    "type": "object",
    "allowedParents": [],
    "properties": {
      "component": {
        "const": "Surface"
      },
      "child": {
        "const": "root"
      }
    },
    "additionalProperties": false
  },
  "FunctionResponse": {
    "type": "object",
    "description": "The return response matching a callAgentFunction or callRendererFunction invocation.",
    "properties": {
      "functionCallId": {
        "$ref": "#/$defs/CallId",
        "description": "The unique ID matching the initiating function call."
      },
      "value": {
        "description": "The return value of the function."
      },
      "error": {
        "type": "object",
        "description": "An error object indicating failure of the function execution.",
        "properties": {
          "code": {
            "type": "string"
          },
          "message": {
            "type": "string"
          }
        },
        "required": ["code", "message"],
        "additionalProperties": false
      }
    },
    "required": ["functionCallId"],
    "oneOf": [
      {
        "required": ["value"]
      },
      {
        "required": ["error"]
      }
    ],
    "additionalProperties": false
  }
};

// node_modules/@a2ui/web_core/src/catalog/spec_shape.js
var DEFAULT_COMMON_TYPES_URI = "common_types.json";
var STANDARD_DEFS_BY_VERSION = {
  "0.8": V08_STANDARD_DEFS,
  "0.9": V09_STANDARD_DEFS,
  "0.9.1": V09_STANDARD_DEFS,
  "1.0": V10_STANDARD_DEFS
};
function standardDefsForVersion(version) {
  if (version) {
    const key = toCanonicalVersion(version) ?? normalizeVersionString(version);
    if (key in STANDARD_DEFS_BY_VERSION)
      return STANDARD_DEFS_BY_VERSION[key];
    if (isAtLeastVersion(version, "1.0"))
      return V10_STANDARD_DEFS;
  }
  return V09_STANDARD_DEFS;
}
function copyJson(value) {
  return JSON.parse(JSON.stringify(value));
}
function isObject2(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
var MAX_DISJOINT_DEPTH = 8;
function jsonTypeOf(value) {
  if (value === null)
    return "null";
  if (Array.isArray(value))
    return "array";
  return typeof value;
}
function resolveSchema(schema, resolve, depth) {
  let node = schema;
  for (let hops = 0; isObject2(node) && typeof node["$ref"] === "string"; hops++) {
    if (hops + depth > MAX_DISJOINT_DEPTH)
      return void 0;
    node = resolve(node["$ref"]);
  }
  return isObject2(node) ? node : void 0;
}
function acceptedTypes(schema) {
  const type = schema["type"];
  if (typeof type === "string" || Array.isArray(type)) {
    return new Set([].concat(type).map((t7) => t7 === "integer" ? "number" : String(t7)));
  }
  if (Object.hasOwn(schema, "const"))
    return /* @__PURE__ */ new Set([jsonTypeOf(schema["const"])]);
  if (Array.isArray(schema["enum"]))
    return new Set(schema["enum"].map(jsonTypeOf));
  return void 0;
}
function acceptedValues(schema) {
  let values;
  if (Object.hasOwn(schema, "const"))
    values = [schema["const"]];
  else if (Array.isArray(schema["enum"]))
    values = schema["enum"];
  else
    return void 0;
  if (values.some((v3) => typeof v3 === "object" && v3 !== null))
    return void 0;
  return values.map((v3) => JSON.stringify(v3));
}
function requiredKeys(schema, resolve, depth) {
  const keys = /* @__PURE__ */ new Set();
  if (Array.isArray(schema["required"])) {
    for (const key of schema["required"])
      keys.add(String(key));
  }
  if (depth < MAX_DISJOINT_DEPTH && Array.isArray(schema["allOf"])) {
    for (const member of schema["allOf"]) {
      const resolved = resolveSchema(member, resolve, depth + 1);
      if (resolved)
        for (const key of requiredKeys(resolved, resolve, depth + 1))
          keys.add(key);
    }
  }
  return keys;
}
function closedKeys(schema) {
  if (schema["additionalProperties"] !== false)
    return void 0;
  if (Object.hasOwn(schema, "patternProperties"))
    return void 0;
  const properties = schema["properties"];
  return new Set(isObject2(properties) ? Object.keys(properties) : []);
}
function propertySchema(schema, key) {
  const properties = schema["properties"];
  return isObject2(properties) ? properties[key] : void 0;
}
function provablyDisjoint(a4, b4, resolve, depth = 0) {
  if (depth > MAX_DISJOINT_DEPTH)
    return false;
  const left = resolveSchema(a4, resolve, depth);
  const right = resolveSchema(b4, resolve, depth);
  if (!left || !right)
    return false;
  const leftTypes = acceptedTypes(left);
  const rightTypes = acceptedTypes(right);
  if (leftTypes && rightTypes && ![...leftTypes].some((t7) => rightTypes.has(t7)))
    return true;
  const leftValues = acceptedValues(left);
  const rightValues = acceptedValues(right);
  if (leftValues && rightValues && !leftValues.some((v3) => rightValues.includes(v3)))
    return true;
  const objectOnly = (types) => types !== void 0 && types.size === 1 && types.has("object");
  if (!objectOnly(leftTypes) || !objectOnly(rightTypes))
    return false;
  const leftRequired = requiredKeys(left, resolve, depth);
  const rightRequired = requiredKeys(right, resolve, depth);
  const leftClosed = closedKeys(left);
  const rightClosed = closedKeys(right);
  if (leftClosed && [...rightRequired].some((key) => !leftClosed.has(key)))
    return true;
  if (rightClosed && [...leftRequired].some((key) => !rightClosed.has(key)))
    return true;
  return [...leftRequired].some((key) => rightRequired.has(key) && provablyDisjoint(propertySchema(left, key), propertySchema(right, key), resolve, depth + 1));
}
function defOf(schema) {
  return schema._def;
}
var ENVELOPE_PROPERTIES = /* @__PURE__ */ new Set(["id", "component"]);
function isSpecComponentSchema(schema) {
  return collectAnnotation(schema).mixins !== void 0;
}
function isSpecFunctionSchema(schema) {
  return Object.keys(collectAnnotation(schema)).length > 0;
}
function schemaRequiresArgs(schema, defs, visited = /* @__PURE__ */ new Set()) {
  if (!isObject2(schema) || schema["default"] !== void 0)
    return false;
  if (Array.isArray(schema["required"]) && schema["required"].length > 0)
    return true;
  if (typeof schema["minProperties"] === "number" && schema["minProperties"] > 0)
    return true;
  const ref = schema["$ref"];
  if (typeof ref === "string" && defs && ref.startsWith("#/$defs/") && !visited.has(ref)) {
    let target = defs;
    for (const raw of ref.slice("#/$defs/".length).split("/")) {
      const segment = raw.replace(/~1/g, "/").replace(/~0/g, "~");
      target = isObject2(target) && Object.hasOwn(target, segment) ? target[segment] : void 0;
    }
    if (target !== void 0 && schemaRequiresArgs(target, defs, new Set(visited).add(ref))) {
      return true;
    }
  }
  if (Array.isArray(schema["allOf"]) && schema["allOf"].some((sub) => schemaRequiresArgs(sub, defs, visited))) {
    return true;
  }
  if (Array.isArray(schema["oneOf"]) && schema["oneOf"].length > 0 && schema["oneOf"].every((sub) => schemaRequiresArgs(sub, defs, visited))) {
    return true;
  }
  if (Array.isArray(schema["anyOf"]) && schema["anyOf"].length > 0 && schema["anyOf"].every((sub) => schemaRequiresArgs(sub, defs, visited))) {
    return true;
  }
  return false;
}
function collectAnnotation(schema, stopAt) {
  const chain = [];
  const stopAnnotation = stopAt ? getSpecAnnotation(stopAt) : void 0;
  let node = schema;
  while (node && typeof node === "object" && "_def" in node) {
    const annotation = getSpecAnnotation(node);
    if (stopAt !== void 0 && (node === stopAt || annotation && annotation === stopAnnotation)) {
      break;
    }
    if (annotation)
      chain.push(annotation);
    const next = innerSchema(node);
    if (!next)
      break;
    node = next;
  }
  return chain.reduceRight((merged, a4) => ({ ...merged, ...a4 }), {});
}
function innerSchema(schema) {
  const def = defOf(schema);
  switch (def.typeName) {
    case "ZodOptional":
    case "ZodDefault":
    case "ZodCatch":
    case "ZodReadonly":
      return def["innerType"];
    case "ZodEffects":
      return def["schema"];
    case "ZodBranded":
      return def["type"];
    default:
      return void 0;
  }
}
var SpecShaper = class {
  /**
   * @param protocolVersion The catalog's protocol version.
   * @param commonTypesUri How the catalog's document writes the common types
   *     document in references, such as `common_types.json`.
   */
  constructor(protocolVersion, commonTypesUri = DEFAULT_COMMON_TYPES_URI) {
    this.commonTypesUri = commonTypesUri;
    this.standardDefsInCatalog = /* @__PURE__ */ new Map();
    this.isV08 = !isAtLeastVersion(protocolVersion, "0.9");
    this.isV10 = isAtLeastVersion(protocolVersion, "1.0");
    this.standardDefs = standardDefsForVersion(protocolVersion);
  }
  /**
   * Returns the definition a catalog document writes for a component.
   *
   * @param name The component's name.
   * @param schema The component's schema, annotated with its mixins.
   * @param allowedParents The component's allowed parents, if restricted.
   * @param allowedChildren The component's allowed children, if restricted.
   */
  componentSchema(name, schema, allowedParents, allowedChildren) {
    const catalogDefs = /* @__PURE__ */ new Map();
    let result;
    if (this.isV08) {
      result = this.toJson(schema, catalogDefs);
      const properties = result["properties"];
      if (isObject2(properties)) {
        for (const key of ENVELOPE_PROPERTIES)
          delete properties[key];
      }
      if (Array.isArray(result["required"])) {
        const required = result["required"].filter((r8) => !ENVELOPE_PROPERTIES.has(r8));
        if (required.length > 0)
          result["required"] = required;
        else
          delete result["required"];
      }
    } else {
      const annotation = collectAnnotation(schema);
      const mixins = annotation.mixins ?? [];
      const inherited = new Set(ENVELOPE_PROPERTIES);
      const refs = [];
      for (const mixin of mixins) {
        refs.push({ $ref: this.mixinRef(mixin, catalogDefs) });
        for (const key of this.mixinProperties(mixin))
          inherited.add(key);
      }
      const object = coreSchema(schema);
      const description = plainDescription(schema);
      const own = this.objectSchema(object, annotation, (key) => !inherited.has(key), catalogDefs);
      const properties = { component: { const: name }, ...own["properties"] };
      const required = ["component", ...own["required"] ?? []];
      const inner = { type: "object" };
      if (description !== void 0 && !this.isV10)
        inner["description"] = description;
      inner["properties"] = properties;
      inner["required"] = required;
      result = { type: "object" };
      if (this.isV10) {
        if (description !== void 0)
          result["description"] = description;
        if (refs.length > 0) {
          result["allOf"] = [...refs, inner];
        } else {
          result["properties"] = properties;
          result["required"] = required;
        }
      } else {
        result["allOf"] = [...refs, inner];
        result["unevaluatedProperties"] = false;
      }
    }
    if (allowedParents && allowedParents.length > 0)
      result["allowedParents"] = [...allowedParents];
    if (allowedChildren && allowedChildren.length > 0) {
      result["allowedChildren"] = [...allowedChildren];
    }
    return { schema: result, catalogDefs };
  }
  /**
   * Returns the definition a catalog document writes for a function: the
   * schema of a call to it.
   *
   * @param name The function's name.
   * @param fn The function's API, whose `schema` is its argument model.
   */
  functionSchema(name, fn, defs, catalogDefs) {
    const args = this.toJson(fn.schema, catalogDefs);
    const argsRequired = schemaRequiresArgs(args, defs);
    const result = { type: "object" };
    if (fn.description)
      result["description"] = fn.description;
    if (this.isV10) {
      result["returnType"] = fn.returnType;
      if (fn.allowedCallers)
        result["allowedCallers"] = fn.allowedCallers;
      if (fn.requiresUserActivation)
        result["requiresUserActivation"] = true;
      result["properties"] = { "@call": { const: name }, args };
      result["required"] = argsRequired ? ["@call", "args"] : ["@call"];
      return result;
    }
    if (fn.allowedCallers)
      result["allowedCallers"] = fn.allowedCallers;
    if (fn.requiresUserActivation)
      result["requiresUserActivation"] = true;
    result["properties"] = { call: { const: name }, args, returnType: { const: fn.returnType } };
    result["required"] = argsRequired ? ["call", "args"] : ["call"];
    result["unevaluatedProperties"] = false;
    return result;
  }
  /**
   * Returns the definition a catalog document writes for a model, such as a
   * mixin or a theme.
   *
   * @param schema The model.
   */
  defSchema(schema, catalogDefs) {
    return this.toJson(schema, catalogDefs, true);
  }
  /** Returns the reference a component's definition makes to a mixin. */
  mixinRef(mixin, catalogDefs) {
    if (typeof mixin === "string")
      return `${this.commonTypesUri}#/$defs/${mixin}`;
    const name = collectAnnotation(mixin).defName;
    if (name === void 0) {
      throw new Error("A component mixin model must be named with specDef().");
    }
    catalogDefs.set(name, mixin);
    return `#/$defs/${name}`;
  }
  /** Returns the names of the properties a mixin declares. */
  mixinProperties(mixin) {
    if (typeof mixin !== "string") {
      const core = coreSchema(mixin);
      return defOf(core).typeName === "ZodObject" ? Object.keys(core.shape) : [];
    }
    const names = [];
    const collect = (def, seen) => {
      if (!isObject2(def) || seen.has(def))
        return;
      seen.add(def);
      if (isObject2(def["properties"]))
        names.push(...Object.keys(def["properties"]));
      for (const sub of Array.isArray(def["allOf"]) ? def["allOf"] : []) {
        const ref = isObject2(sub) ? sub["$ref"] : void 0;
        const target = typeof ref === "string" && ref.startsWith("#/$defs/") ? this.standardDefs[ref.substring("#/$defs/".length)] : sub;
        collect(target, seen);
      }
    };
    collect(this.standardDefs[mixin], /* @__PURE__ */ new Set());
    return names;
  }
  /**
   * Returns the JSON Schema a catalog document writes for a Zod schema.
   *
   * @param schema The schema.
   * @param catalogDefs Optional map collecting referenced catalog `$defs` models.
   * @param isDefRoot Whether `schema` is the root of a `$defs` entry being serialized.
   */
  toJson(schema, catalogDefs, isDefRoot = false) {
    const annotation = collectAnnotation(schema);
    const description = schemaDescription(schema);
    const defaultValue = schemaDefault(schema);
    let effectiveAnnotation = annotation;
    let effectiveDefault = defaultValue;
    let core;
    let ownDescription = description;
    if (!isDefRoot && catalogDefs !== void 0 && annotation.defName !== void 0 && annotation.defModel !== void 0) {
      catalogDefs.set(annotation.defName, annotation.defModel);
      core = { $ref: `#/$defs/${annotation.defName}` };
      effectiveAnnotation = collectAnnotation(schema, annotation.defModel);
      const defDesc = schemaDescription(annotation.defModel);
      ownDescription = description !== defDesc ? description : void 0;
      const defDefault = schemaDefault(annotation.defModel);
      effectiveDefault = defaultValue !== defDefault ? defaultValue : void 0;
    } else if (description !== void 0 && description.startsWith("REF:")) {
      const content = description.substring(4);
      const pipe = content.indexOf("|");
      const ref = pipe === -1 ? content : content.substring(0, pipe);
      ownDescription = pipe === -1 ? void 0 : content.substring(pipe + 1);
      core = { $ref: this.refTarget(ref) };
    } else {
      core = this.coreJson(coreSchema(schema), annotation, catalogDefs);
    }
    if (effectiveAnnotation.keywords)
      Object.assign(core, copyJson(effectiveAnnotation.keywords));
    const result = effectiveAnnotation.allOf ? { allOf: [core, ...effectiveAnnotation.allOf.map((s5) => copyJson(s5))] } : core;
    if (ownDescription)
      result["description"] = ownDescription;
    if (effectiveDefault !== void 0)
      result["default"] = effectiveDefault;
    return result;
  }
  /**
   * Returns the standard definition a reference in the catalog document names
   * (`<commonTypesUri>#/$defs/<name>`), or `undefined` for any other
   * reference. A bare `#/$defs/<name>` in the catalog document names one of
   * the catalog's own definitions, which may share a standard definition's
   * name, so it is not followed.
   *
   * The definition is returned with its own references, which are local to
   * the common types document, written as the catalog document writes them,
   * so that they resolve against the same document when followed in turn.
   */
  resolveStandardRef(ref) {
    const prefix = `${this.commonTypesUri}#/$defs/`;
    if (!ref.startsWith(prefix))
      return void 0;
    const name = ref.substring(prefix.length);
    if (name.includes("/") || !Object.hasOwn(this.standardDefs, name))
      return void 0;
    const cached = this.standardDefsInCatalog.get(name);
    if (cached)
      return cached;
    const definition = this.standardDefs[name];
    if (!isObject2(definition))
      return void 0;
    const rewritten = this.withCatalogRefs(definition);
    this.standardDefsInCatalog.set(name, rewritten);
    return rewritten;
  }
  /**
   * Returns a copy of a part of the common types document with its local
   * references written as the catalog document writes them.
   */
  withCatalogRefs(value) {
    if (Array.isArray(value))
      return value.map((item) => this.withCatalogRefs(item));
    if (!isObject2(value))
      return value;
    const result = {};
    for (const [key, item] of Object.entries(value)) {
      result[key] = key === "$ref" && typeof item === "string" ? this.refTarget(item) : this.withCatalogRefs(item);
    }
    return result;
  }
  /** Writes a `REF:` marker's reference as the catalog document does. */
  refTarget(ref) {
    if (!ref.startsWith("#/$defs/"))
      return ref;
    return `${this.commonTypesUri}${ref}`;
  }
  /** Converts the core of a schema, below its wrappers. */
  coreJson(schema, annotation, catalogDefs) {
    const def = defOf(schema);
    switch (def.typeName) {
      case "ZodObject":
        return this.objectSchema(schema, annotation, () => true, catalogDefs);
      case "ZodString":
        return stringSchema(def);
      case "ZodNumber":
        return numberSchema(def);
      case "ZodBoolean":
        return { type: "boolean" };
      case "ZodNull":
        return { type: "null" };
      case "ZodEnum":
        return { type: "string", enum: [...def["values"]] };
      case "ZodLiteral":
        return { const: def["value"] };
      case "ZodAny":
      case "ZodUnknown":
        return {};
      case "ZodNever":
        return { not: {} };
      case "ZodArray": {
        const result = {
          type: "array",
          items: this.toJson(def["type"], catalogDefs)
        };
        const min = def["minLength"];
        const max = def["maxLength"];
        if (min)
          result["minItems"] = min.value;
        if (max)
          result["maxItems"] = max.value;
        return result;
      }
      case "ZodUnion":
      case "ZodDiscriminatedUnion": {
        const options = def["options"];
        const branches = [...options].map((option) => this.toJson(option, catalogDefs));
        const resolve = (ref) => {
          if (ref.startsWith("#/$defs/") && catalogDefs !== void 0) {
            const defName = ref.substring("#/$defs/".length);
            const defModel = catalogDefs.get(defName);
            if (defModel !== void 0)
              return this.defSchema(defModel, catalogDefs);
          }
          return this.resolveStandardRef(ref);
        };
        const disjoint = branches.every((branch, i7) => branches.slice(i7 + 1).every((other) => provablyDisjoint(branch, other, resolve)));
        return { [disjoint ? "oneOf" : "anyOf"]: branches };
      }
      case "ZodRecord":
        return {
          type: "object",
          additionalProperties: this.toJson(def["valueType"], catalogDefs)
        };
      case "ZodLazy":
        return this.toJson(def["getter"](), catalogDefs);
      default:
        return fallbackJson(schema);
    }
  }
  /**
   * Converts an object schema, keeping the properties `keep` accepts. Its
   * `required` list follows the annotation, or else the property order.
   */
  objectSchema(schema, annotation, keep, catalogDefs) {
    const def = defOf(schema);
    if (def.typeName !== "ZodObject")
      return this.toJson(schema, catalogDefs);
    const shape = schema.shape;
    const properties = {};
    const required = [];
    for (const [key, value] of Object.entries(shape)) {
      if (!keep(key))
        continue;
      properties[key] = this.toJson(value, catalogDefs);
      if (!value.isOptional())
        required.push(key);
    }
    const result = { type: "object", properties };
    const order = annotation.required?.filter((key) => required.includes(key));
    const unordered = required.filter((key) => !order?.includes(key));
    if (required.length > 0)
      result["required"] = order ? [...order, ...unordered] : required;
    const unknownKeys = def["unknownKeys"];
    const catchall = def["catchall"];
    const keyword = annotation.closure ?? (unknownKeys === "strict" ? "additionalProperties" : "none");
    if (catchall && defOf(catchall).typeName !== "ZodNever") {
      result[keyword === "none" ? "additionalProperties" : keyword] = this.toJson(catchall, catalogDefs);
    } else if (keyword !== "none") {
      result[keyword] = unknownKeys === "passthrough";
    }
    return result;
  }
};
function coreSchema(schema) {
  let node = schema;
  for (let next = innerSchema(node); next; next = innerSchema(node))
    node = next;
  return node;
}
function schemaDescription(schema) {
  for (let node = schema; node; node = innerSchema(node)) {
    const description = defOf(node).description;
    if (typeof description === "string")
      return description;
  }
  return void 0;
}
function plainDescription(schema) {
  const description = schemaDescription(schema);
  return description !== void 0 && !description.startsWith("REF:") ? description : void 0;
}
function schemaDefault(schema) {
  for (let node = schema; node; node = innerSchema(node)) {
    const def = defOf(node);
    if (def.typeName === "ZodDefault")
      return def["defaultValue"]();
  }
  return void 0;
}
var STRING_FORMATS = {
  email: "email",
  url: "uri",
  uuid: "uuid",
  datetime: "date-time",
  date: "date",
  time: "time"
};
function stringSchema(def) {
  const result = { type: "string" };
  for (const check of def["checks"] ?? []) {
    if (check.kind === "regex")
      result["pattern"] = check["regex"].source;
    else if (check.kind === "min")
      result["minLength"] = check["value"];
    else if (check.kind === "max")
      result["maxLength"] = check["value"];
    else if (check.kind in STRING_FORMATS)
      result["format"] = STRING_FORMATS[check.kind];
  }
  return result;
}
function numberSchema(def) {
  const checks = def["checks"] ?? [];
  const result = {
    type: checks.some((check) => check.kind === "int") ? "integer" : "number"
  };
  for (const check of checks) {
    if (check.kind === "min") {
      result[check["inclusive"] ? "minimum" : "exclusiveMinimum"] = check["value"];
    } else if (check.kind === "max") {
      result[check["inclusive"] ? "maximum" : "exclusiveMaximum"] = check["value"];
    }
  }
  return result;
}
function fallbackJson(schema) {
  const json = zodToJsonSchema(schema, {
    target: "jsonSchema2019-09",
    $refStrategy: "none",
    override: exclusiveUnionOverride
  });
  delete json["$schema"];
  delete json["definitions"];
  delete json["$defs"];
  return json;
}

// node_modules/@a2ui/web_core/src/catalog/schema_serializer.js
var UNION_DEF_NAMES = /* @__PURE__ */ new Set(["anyComponent", "anyFunction"]);
var THEME_DOCUMENT_KEYS = ["theme", "themeSchema", "styles"];
function deepCopyJson(value) {
  if (value === void 0)
    return value;
  return JSON.parse(JSON.stringify(value));
}
function deepFreeze(value) {
  if (typeof value === "object" && value !== null && !Object.isFrozen(value)) {
    Object.freeze(value);
    for (const child of Object.values(value))
      deepFreeze(child);
  }
  return value;
}
function frozenJson(value) {
  if (typeof value !== "object" || value === null || Object.isFrozen(value))
    return value;
  return deepFreeze(deepCopyJson(value));
}
function isObject3(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
function collectLocalDefNames(node, found) {
  if (Array.isArray(node)) {
    for (const item of node)
      collectLocalDefNames(item, found);
    return;
  }
  if (!isObject3(node))
    return;
  const ref = node.$ref;
  if (typeof ref === "string" && ref.startsWith("#/$defs/")) {
    found.add(ref.slice("#/$defs/".length).split("/")[0].replace(/~1/g, "/").replace(/~0/g, "~"));
  }
  for (const value of Object.values(node))
    collectLocalDefNames(value, found);
}
function reachableDefs(roots, defs) {
  const reached = /* @__PURE__ */ new Set();
  const pending = /* @__PURE__ */ new Set();
  for (const root of roots)
    collectLocalDefNames(root, pending);
  while (pending.size > 0) {
    const [name] = pending;
    pending.delete(name);
    if (reached.has(name))
      continue;
    reached.add(name);
    if (name in defs)
      collectLocalDefNames(defs[name], pending);
  }
  return reached;
}
function entryDefinition(entry, defs) {
  const definition = loadedDefinition(entry);
  if (!isObject3(definition))
    return void 0;
  const available = defs ?? {};
  for (const name of reachableDefs([definition], available)) {
    if (!(name in available) && !UNION_DEF_NAMES.has(name))
      return void 0;
  }
  return definition;
}
function isFunctionListItem(definition) {
  return typeof definition["name"] === "string" && !("properties" in definition);
}
function retainedDefs(defs, before, after, extensionFields) {
  if (!defs)
    return defs;
  const definitions2 = (entries) => entries.map((entry) => entryDefinition(entry, defs)).filter((d4) => d4 !== void 0);
  const reachedBefore = reachableDefs(definitions2(before), defs);
  const unconditional = Object.keys(defs).filter((name) => !reachedBefore.has(name));
  const reachedAfter = reachableDefs([...definitions2(after), extensionFields, ...unconditional.map((name) => defs[name])], defs);
  const kept = {};
  let dropped = false;
  for (const [name, def] of Object.entries(defs)) {
    if (!reachedBefore.has(name) || reachedAfter.has(name)) {
      kept[name] = def;
    } else {
      dropped = true;
    }
  }
  return dropped ? kept : defs;
}
function rewriteRefMarkers(node, localDefs, commonTypesUri = DEFAULT_COMMON_TYPES_URI) {
  if (Array.isArray(node)) {
    for (const item of node)
      rewriteRefMarkers(item, localDefs, commonTypesUri);
    return;
  }
  if (!isObject3(node))
    return;
  if (typeof node.description === "string" && node.description.startsWith("REF:")) {
    const content = node.description.substring(4);
    const pipeIndex = content.indexOf("|");
    const ref = pipeIndex === -1 ? content : content.substring(0, pipeIndex);
    const desc = pipeIndex === -1 ? "" : content.substring(pipeIndex + 1);
    const savedDefault = node.default;
    for (const key of Object.keys(node))
      delete node[key];
    const pointer = ref.split(/#\/(?:\$defs|definitions)\//)[1];
    const defName = pointer?.split("/")[0];
    node.$ref = defName === void 0 ? ref : ref.startsWith("#") && localDefs.has(defName) ? ref : `${commonTypesUri}#/$defs/${pointer}`;
    if (savedDefault !== void 0)
      node.default = savedDefault;
    if (desc)
      node.description = desc;
    return;
  }
  for (const key of ["additionalProperties", "unevaluatedProperties"]) {
    const value = node[key];
    if (isObject3(value) && Object.keys(value).length === 0)
      node[key] = true;
  }
  delete node.$schema;
  for (const value of Object.values(node))
    rewriteRefMarkers(value, localDefs, commonTypesUri);
}
function zodToUnbundledJson(schema, localDefs, commonTypesUri = DEFAULT_COMMON_TYPES_URI) {
  if (!schema || typeof schema !== "object" || !("safeParse" in schema)) {
    return { type: "object", properties: {} };
  }
  const json = zodToJsonSchema(schema, {
    target: "jsonSchema2019-09",
    $refStrategy: "none",
    override: exclusiveUnionOverride
  });
  delete json.definitions;
  delete json.$defs;
  rewriteRefMarkers(json, localDefs, commonTypesUri);
  return json;
}
function closeObjectSchema(json) {
  if (json.type !== "object")
    return;
  const extra = json.unevaluatedProperties ?? json.additionalProperties;
  delete json.additionalProperties;
  json.unevaluatedProperties = typeof extra === "boolean" || isObject3(extra) ? extra : false;
}
function serializeComponentModel(name, comp, localDefs, commonTypesUri = DEFAULT_COMMON_TYPES_URI) {
  const json = zodToUnbundledJson(comp.schema, localDefs, commonTypesUri);
  const { component: _component, id: _id, ...props } = isObject3(json.properties) ? json.properties : {};
  const required = Array.isArray(json.required) ? json.required.filter((r8) => r8 !== "component" && r8 !== "id") : [];
  const extra = json.unevaluatedProperties ?? json.additionalProperties;
  const result = {
    type: "object",
    properties: { component: { const: name }, ...props },
    required: ["component", ...required],
    unevaluatedProperties: typeof extra === "boolean" || isObject3(extra) ? extra : false
  };
  if (comp.allowedParents && comp.allowedParents.length > 0) {
    result.allowedParents = [...comp.allowedParents];
  }
  if (comp.allowedChildren && comp.allowedChildren.length > 0) {
    result.allowedChildren = [...comp.allowedChildren];
  }
  if (comp.metadata !== void 0) {
    result.metadata = deepCopyJson(comp.metadata);
  }
  return result;
}
function serializeFunctionModel(name, fn, localDefs, isAtLeastV10, asListItem, commonTypesUri = DEFAULT_COMMON_TYPES_URI, defs) {
  const args = zodToUnbundledJson(fn.schema, localDefs, commonTypesUri);
  if (asListItem) {
    return {
      name,
      ...fn.description ? { description: fn.description } : {},
      returnType: fn.returnType,
      parameters: args,
      ...fn.allowedCallers ? { allowedCallers: fn.allowedCallers } : {},
      ...fn.requiresUserActivation !== void 0 ? { requiresUserActivation: fn.requiresUserActivation } : {}
    };
  }
  closeObjectSchema(args);
  const argsRequired = schemaRequiresArgs(args, defs);
  const result = { type: "object" };
  if (fn.description)
    result.description = fn.description;
  if (isAtLeastV10) {
    result.returnType = fn.returnType;
    if (fn.allowedCallers)
      result.allowedCallers = fn.allowedCallers;
    if (fn.requiresUserActivation !== void 0) {
      result.requiresUserActivation = fn.requiresUserActivation;
    }
    result.properties = { "@call": { const: name }, args };
    result.required = argsRequired ? ["@call", "args"] : ["@call"];
    return result;
  }
  if (fn.allowedCallers)
    result.allowedCallers = fn.allowedCallers;
  if (fn.requiresUserActivation !== void 0) {
    result.requiresUserActivation = fn.requiresUserActivation;
  }
  result.properties = { call: { const: name }, args, returnType: { const: fn.returnType } };
  result.required = argsRequired ? ["call", "args"] : ["call"];
  result.unevaluatedProperties = false;
  return result;
}
function serializeThemeModel(theme, localDefs, commonTypesUri = DEFAULT_COMMON_TYPES_URI) {
  const json = zodToUnbundledJson(theme, localDefs, commonTypesUri);
  return {
    type: "object",
    properties: isObject3(json.properties) ? json.properties : {},
    ...Array.isArray(json.required) && json.required.length > 0 ? { required: json.required } : {},
    additionalProperties: json.additionalProperties !== void 0 ? json.additionalProperties : true
  };
}
var EMPTY_UNION = { not: {} };
function buildComponentUnion(names) {
  const oneOf = [...names].map((name) => ({ $ref: `#/components/${escapePointer(name)}` }));
  if (oneOf.length === 0)
    return { ...EMPTY_UNION };
  return { oneOf, discriminator: { propertyName: "component" } };
}
function buildFunctionUnion(names) {
  const oneOf = [...names].map((name) => ({ $ref: `#/functions/${escapePointer(name)}` }));
  return oneOf.length === 0 ? { ...EMPTY_UNION } : { oneOf };
}
function escapePointer(token) {
  return token.replace(/~/g, "~0").replace(/\//g, "~1");
}
var documentModels = /* @__PURE__ */ new WeakMap();
function catalogDocumentModel(catalog) {
  let model = documentModels.get(catalog);
  if (!model) {
    model = buildDocumentModel(catalog);
    documentModels.set(catalog, model);
  }
  return model;
}
function buildDocumentModel(catalog) {
  const shaper = new SpecShaper(catalog.protocolVersion, catalog.commonTypesUri);
  const computedDefs = /* @__PURE__ */ new Map();
  const shapedComponents = /* @__PURE__ */ new Map();
  for (const [name, comp] of catalog.components) {
    if (loadedDefinition(comp) || !isSpecComponentSchema(comp.schema))
      continue;
    const { schema, catalogDefs } = shaper.componentSchema(name, comp.schema, comp.allowedParents, comp.allowedChildren);
    if (comp.metadata !== void 0) {
      schema["metadata"] = deepCopyJson(comp.metadata);
    }
    shapedComponents.set(name, deepFreeze(schema));
    for (const [defName, defModel] of catalogDefs)
      computedDefs.set(defName, defModel);
  }
  for (const defModel of catalog.defModels ?? []) {
    const defName = getSpecAnnotation(defModel)?.defName;
    if (defName === void 0) {
      throw new Error("A catalog definition model must be named with specDef().");
    }
    computedDefs.set(defName, defModel);
  }
  const functions = /* @__PURE__ */ new Map();
  const rawFunctions = /* @__PURE__ */ new Map();
  for (const [name, fn] of catalog.functions) {
    if (!loadedDefinition(fn) && isSpecFunctionSchema(fn.schema)) {
      rawFunctions.set(name, fn);
      shaper.toJson(fn.schema, computedDefs);
    }
  }
  const defs = { ...catalog.defs };
  const serializedDefs = /* @__PURE__ */ new Set();
  while ([...computedDefs.keys()].some((defName) => !serializedDefs.has(defName) && !(defName in defs))) {
    for (const [defName, defModel] of [...computedDefs]) {
      if (serializedDefs.has(defName) || defName in defs)
        continue;
      serializedDefs.add(defName);
      defs[defName] = shaper.defSchema(defModel, computedDefs);
    }
  }
  const extensionFields = { ...catalog.extensionFields };
  if (catalog.themeLocation === "styles" && catalog.themeSchema && !hasAuthoredTheme(catalog)) {
    const theme = shaper.defSchema(catalog.themeSchema);
    extensionFields["styles"] = isObject3(theme["properties"]) ? theme["properties"] : {};
  }
  const components = /* @__PURE__ */ new Map();
  for (const [name, comp] of catalog.components) {
    components.set(name, shapedComponents.get(name) ?? entryDefinition(comp, defs));
  }
  for (const [name, fn] of catalog.functions) {
    const loaded = entryDefinition(fn, defs);
    functions.set(name, loaded ?? (rawFunctions.has(name) ? deepFreeze(shaper.functionSchema(name, fn, defs, computedDefs)) : void 0));
  }
  return {
    components,
    functions,
    defs: deepFreeze(defs),
    extensionFields: deepFreeze(extensionFields)
  };
}
function hasAuthoredTheme(catalog) {
  return isObject3(catalog.defs?.["theme"]) || THEME_DOCUMENT_KEYS.some((key) => catalog.extensionFields?.[key] !== void 0);
}
function serializeCatalogDocument(catalog) {
  const isAtLeastV10 = isAtLeastVersion(catalog.protocolVersion, "1.0");
  const model = catalogDocumentModel(catalog);
  const localDefs = new Set(Object.keys(model.defs));
  const doc = {};
  if (catalog.schemaUri !== void 0)
    doc.$schema = catalog.schemaUri;
  if (catalog.schemaId !== void 0)
    doc.$id = catalog.schemaId;
  if (typeof catalog.declaredProtocolVersion === "string") {
    doc.protocolVersion = catalog.declaredProtocolVersion;
  } else if (catalog.declaredProtocolVersion === void 0) {
    doc.protocolVersion = toCanonicalVersion(catalog.protocolVersion) ?? catalog.protocolVersion;
  }
  if (catalog.title !== void 0)
    doc.title = catalog.title;
  if (catalog.description !== void 0)
    doc.description = catalog.description;
  if (catalog.declaresCatalogId !== false)
    doc.catalogId = catalog.id;
  if (catalog.instructions !== void 0)
    doc.instructions = catalog.instructions;
  const commonTypesUri = catalog.commonTypesUri ?? DEFAULT_COMMON_TYPES_URI;
  if (catalog.declaresComponents !== false || catalog.components.size > 0) {
    const components = {};
    for (const [name, comp] of catalog.components) {
      const definition = model.components.get(name);
      components[name] = definition ? deepCopyJson(definition) : serializeComponentModel(name, comp, localDefs, commonTypesUri);
    }
    doc.components = components;
  }
  const functionEntries = [...catalog.functions].filter(([name, fn]) => loadedDefinition(fn) !== void 0 || !name.startsWith("@"));
  const functionNames = functionEntries.map(([name]) => name);
  if (catalog.functionsLayout === "list") {
    doc.functions = functionEntries.map(([name, fn]) => {
      const definition = model.functions.get(name);
      return definition && isFunctionListItem(definition) ? deepCopyJson(definition) : serializeFunctionModel(name, fn, localDefs, isAtLeastV10, true, commonTypesUri, model.defs);
    });
  } else if (catalog.functionsLayout === "map" || functionEntries.length > 0) {
    const functions = {};
    for (const [name, fn] of functionEntries) {
      const definition = model.functions.get(name);
      functions[name] = definition && !isFunctionListItem(definition) ? deepCopyJson(definition) : serializeFunctionModel(name, fn, localDefs, isAtLeastV10, false, commonTypesUri, model.defs);
    }
    doc.functions = functions;
  }
  for (const [key, value] of Object.entries(model.extensionFields)) {
    if (!(key in doc))
      doc[key] = deepCopyJson(value);
  }
  const defs = deepCopyJson({ ...model.defs });
  const themeLocation = catalog.themeLocation ?? (isAtLeastV10 ? void 0 : "defs");
  if (themeLocation === "defs" && catalog.themeSchema && !hasAuthoredTheme(model)) {
    defs.theme = getSpecAnnotation(catalog.themeSchema) ? new SpecShaper(catalog.protocolVersion, catalog.commonTypesUri).defSchema(catalog.themeSchema) : serializeThemeModel(catalog.themeSchema, localDefs, commonTypesUri);
  }
  const unions = catalog.unions ?? {
    anyComponent: isAtLeastV10 || catalog.components.size > 0,
    anyFunction: isAtLeastV10 || functionNames.length > 0
  };
  if (unions.anyComponent && !("anyComponent" in defs)) {
    const savedAnyFunction = defs.anyFunction;
    if (savedAnyFunction !== void 0)
      delete defs.anyFunction;
    defs.anyComponent = buildComponentUnion(catalog.components.keys());
    if (savedAnyFunction !== void 0)
      defs.anyFunction = savedAnyFunction;
  }
  if (unions.anyFunction && !("anyFunction" in defs)) {
    defs.anyFunction = buildFunctionUnion(functionNames);
  }
  if (catalog.defs !== void 0 || Object.keys(defs).length > 0) {
    doc.$defs = defs;
  }
  return doc;
}

// node_modules/@a2ui/web_core/src/catalog/schema_loader.js
var DEFAULT_PROTOCOL_VERSION = "0.9";
function resolveJsonPointer(rootDoc, pointer) {
  if (!pointer.startsWith("#/"))
    return void 0;
  const segments = pointer.slice(2).split("/").map((s5) => s5.replace(/~1/g, "/").replace(/~0/g, "~"));
  let curr = rootDoc;
  for (const seg of segments) {
    if (curr && typeof curr === "object" && seg in curr) {
      curr = curr[seg];
    } else {
      return void 0;
    }
  }
  return typeof curr === "object" && curr !== null ? curr : void 0;
}
function resolveProtocolRef(ref) {
  const defName = ref.split(/#\/(?:\$defs|definitions)\//)[1];
  return defName && Object.hasOwn(CommonSchemas, defName) ? CommonSchemas[defName] : void 0;
}
function convertEnumToZod(values) {
  if (values.length === 0) {
    return external_exports.unknown();
  }
  if (values.every((v3) => typeof v3 === "string")) {
    return external_exports.enum(values);
  }
  if (values.length === 1) {
    return external_exports.literal(values[0]);
  }
  return external_exports.union(values.map((v3) => external_exports.literal(v3)));
}
function canonicalJsonStringify(val) {
  if (val === null || typeof val !== "object") {
    return JSON.stringify(val);
  }
  if (Array.isArray(val)) {
    return `[${val.map(canonicalJsonStringify).join(",")}]`;
  }
  const keys = Object.keys(val).sort();
  const entries = keys.map((k2) => `${JSON.stringify(k2)}:${canonicalJsonStringify(val[k2])}`);
  return `{${entries.join(",")}}`;
}
function finalizePropertyZod(baseZod, propSchema, rootDoc, visitedPointers, defCache) {
  let result = baseZod;
  if (propSchema.not && typeof propSchema.not === "object") {
    const notSchema = convertPropertyToZod(propSchema.not, rootDoc, new Set(visitedPointers), defCache);
    result = result.refine((val) => !notSchema.safeParse(val).success, {
      message: 'Value matched prohibited "not" schema'
    });
  }
  if (propSchema.default !== void 0) {
    result = result.default(propSchema.default);
  }
  if (typeof propSchema.description === "string") {
    result = result.describe(propSchema.description);
  }
  return result;
}
function convertRefToZod(ref, propSchema, rootDoc, visitedPointers, defCache) {
  const resolvedProtocol = resolveProtocolRef(ref);
  if (resolvedProtocol) {
    const defName = ref.split(/#\/(?:\$defs|definitions)\//)[1];
    const desc = typeof propSchema.description === "string" ? `REF:common_types.json#/$defs/${defName}|${propSchema.description}` : resolvedProtocol.description;
    return desc ? resolvedProtocol.describe(desc) : resolvedProtocol;
  }
  if (rootDoc && ref.startsWith("#/")) {
    const localTarget = resolveJsonPointer(rootDoc, ref);
    if (!localTarget) {
      return external_exports.unknown().superRefine((_val, ctx) => {
        ctx.addIssue({
          code: external_exports.ZodIssueCode.custom,
          message: `Unresolvable schema reference: '${ref}'`
        });
      });
    }
    if (visitedPointers.has(ref)) {
      let cached = defCache.get(ref);
      return external_exports.lazy(() => {
        if (!cached) {
          cached = defCache.get(ref) ?? convertPropertyToZod(localTarget, rootDoc, /* @__PURE__ */ new Set([ref]), defCache);
          defCache.set(ref, cached);
        }
        return cached;
      });
    }
    let zodType = defCache.get(ref);
    if (!zodType) {
      const nextVisited = new Set(visitedPointers);
      nextVisited.add(ref);
      zodType = convertPropertyToZod(localTarget, rootDoc, nextVisited, defCache);
      defCache.set(ref, zodType);
    }
    if (typeof propSchema.description === "string") {
      zodType = zodType.describe(propSchema.description);
    }
    return zodType;
  }
  return void 0;
}
function convertUnionToZod(propSchema, rootDoc, visitedPointers, defCache) {
  const isOneOf = Array.isArray(propSchema.oneOf);
  const rawBranches = propSchema.oneOf || propSchema.anyOf;
  const branches = rawBranches.filter((b4) => typeof b4 === "object" && b4 !== null);
  if (branches.length === 0)
    return void 0;
  const enumBranch = branches.find((b4) => Array.isArray(b4.enum));
  const hasBinding = branches.some((b4) => typeof b4.$ref === "string" && b4.$ref.includes("DataBinding"));
  const zodBranches = branches.map((b4) => convertPropertyToZod(b4, rootDoc, new Set(visitedPointers), defCache));
  let unionZod;
  if (zodBranches.length === 1) {
    unionZod = zodBranches[0];
  } else {
    const tuple = [zodBranches[0], zodBranches[1], ...zodBranches.slice(2)];
    unionZod = isOneOf ? exclusiveUnion(tuple) : external_exports.union(tuple);
  }
  if (propSchema.default !== void 0) {
    unionZod = unionZod.default(propSchema.default);
  }
  const desc = (typeof propSchema.description === "string" ? propSchema.description : void 0) || (enumBranch && hasBinding ? "REF:common_types.json#/$defs/DynamicString" : void 0);
  return desc ? unionZod.describe(desc) : unionZod;
}
function convertAllOfPropertyToZod(propSchema, rootDoc, visitedPointers, defCache) {
  const rawBranches = propSchema.allOf;
  const branches = rawBranches.filter((b4) => typeof b4 === "object" && b4 !== null);
  if (branches.length === 0)
    return void 0;
  if (rootDoc) {
    const subSchemas = collectComponentSubSchemas(propSchema, rootDoc, new Set(visitedPointers), false);
    if (subSchemas.length > 0) {
      const requiredSet = /* @__PURE__ */ new Set();
      for (const s5 of subSchemas) {
        if (s5.kind === "json" && Array.isArray(s5.schema.required)) {
          for (const r8 of s5.schema.required) {
            if (typeof r8 === "string")
              requiredSet.add(r8);
          }
        }
      }
      if (Array.isArray(propSchema.required)) {
        for (const r8 of propSchema.required) {
          if (typeof r8 === "string")
            requiredSet.add(r8);
        }
      }
      const shape = {};
      for (const s5 of subSchemas) {
        if (s5.kind === "zod") {
          Object.assign(shape, s5.properties);
          continue;
        }
        const propShape = convertPropertiesToShape(s5.schema.properties || {}, requiredSet, false, rootDoc, new Set(visitedPointers), defCache);
        Object.assign(shape, propShape);
      }
      const baseObj = external_exports.object(shape);
      const forbidExtra = propSchema.additionalProperties === false || propSchema.unevaluatedProperties === false || subSchemas.some((s5) => s5.kind === "json" && (s5.schema.additionalProperties === false || s5.schema.unevaluatedProperties === false));
      const obj = forbidExtra ? baseObj.strict() : baseObj.passthrough();
      return finalizePropertyZod(obj, propSchema, rootDoc, visitedPointers, defCache);
    }
  }
  const zodBranches = branches.map((b4) => convertPropertyToZod(b4, rootDoc, new Set(visitedPointers), defCache));
  let intersected = zodBranches[0];
  for (let i7 = 1; i7 < zodBranches.length; i7++) {
    intersected = intersected.and(zodBranches[i7]);
  }
  return finalizePropertyZod(intersected, propSchema, rootDoc, visitedPointers, defCache);
}
function convertPropertyToZod(propSchema, rootDoc, visitedPointers = /* @__PURE__ */ new Set(), defCache = /* @__PURE__ */ new Map()) {
  if (!propSchema || typeof propSchema !== "object") {
    return external_exports.unknown();
  }
  if (propSchema instanceof external_exports.ZodType) {
    return propSchema;
  }
  if (propSchema.$ref && typeof propSchema.$ref === "string") {
    const resolvedRef = convertRefToZod(propSchema.$ref, propSchema, rootDoc, visitedPointers, defCache);
    if (resolvedRef)
      return resolvedRef;
  }
  if (Array.isArray(propSchema.oneOf) || Array.isArray(propSchema.anyOf)) {
    const resolvedUnion = convertUnionToZod(propSchema, rootDoc, visitedPointers, defCache);
    if (resolvedUnion)
      return resolvedUnion;
  }
  if (Array.isArray(propSchema.allOf) && propSchema.allOf.length > 0) {
    const resolvedAllOf = convertAllOfPropertyToZod(propSchema, rootDoc, visitedPointers, defCache);
    if (resolvedAllOf)
      return resolvedAllOf;
  }
  if (propSchema.const !== void 0) {
    const constZod = external_exports.literal(propSchema.const);
    return finalizePropertyZod(constZod, propSchema, rootDoc, visitedPointers, defCache);
  }
  if (Array.isArray(propSchema.enum) && propSchema.enum.length > 0) {
    const enumZod = convertEnumToZod(propSchema.enum);
    return finalizePropertyZod(enumZod, propSchema, rootDoc, visitedPointers, defCache);
  }
  if (propSchema.type === "array") {
    const itemSchema = propSchema.items && typeof propSchema.items === "object" ? convertPropertyToZod(propSchema.items, rootDoc, new Set(visitedPointers), defCache) : external_exports.unknown();
    let arr = external_exports.array(itemSchema);
    if (typeof propSchema.minItems === "number") {
      arr = arr.min(propSchema.minItems);
    }
    if (typeof propSchema.maxItems === "number") {
      arr = arr.max(propSchema.maxItems);
    }
    if (propSchema.uniqueItems === true) {
      arr = arr.refine((items) => new Set(items.map(canonicalJsonStringify)).size === items.length, { message: "Array items must be unique" });
    }
    return finalizePropertyZod(arr, propSchema, rootDoc, visitedPointers, defCache);
  }
  switch (propSchema.type) {
    case "string": {
      let s5 = external_exports.string();
      if (typeof propSchema.minLength === "number") {
        s5 = s5.min(propSchema.minLength);
      }
      if (typeof propSchema.maxLength === "number") {
        s5 = s5.max(propSchema.maxLength);
      }
      if (typeof propSchema.pattern === "string") {
        try {
          s5 = s5.regex(new RegExp(propSchema.pattern, "u"));
        } catch {
        }
      }
      return finalizePropertyZod(s5, propSchema, rootDoc, visitedPointers, defCache);
    }
    case "integer":
    case "number": {
      let n6 = propSchema.type === "integer" ? external_exports.number().int() : external_exports.number();
      if (typeof propSchema.minimum === "number") {
        n6 = n6.min(propSchema.minimum);
      }
      if (typeof propSchema.maximum === "number") {
        n6 = n6.max(propSchema.maximum);
      }
      if (typeof propSchema.exclusiveMinimum === "number") {
        n6 = n6.gt(propSchema.exclusiveMinimum);
      }
      if (typeof propSchema.exclusiveMaximum === "number") {
        n6 = n6.lt(propSchema.exclusiveMaximum);
      }
      if (typeof propSchema.multipleOf === "number") {
        n6 = n6.multipleOf(propSchema.multipleOf);
      }
      return finalizePropertyZod(n6, propSchema, rootDoc, visitedPointers, defCache);
    }
    case "boolean": {
      return finalizePropertyZod(external_exports.boolean(), propSchema, rootDoc, visitedPointers, defCache);
    }
    case "object": {
      const props = propSchema.properties;
      let obj;
      if (typeof props === "object" && props !== null) {
        const required = Array.isArray(propSchema.required) ? new Set(propSchema.required.filter((r8) => typeof r8 === "string")) : /* @__PURE__ */ new Set();
        const baseObj = external_exports.object(convertPropertiesToShape(props, required, false, rootDoc, visitedPointers, defCache));
        const forbidExtra = propSchema.additionalProperties === false || propSchema.unevaluatedProperties === false;
        obj = forbidExtra ? baseObj.strict() : baseObj.passthrough();
      } else {
        obj = external_exports.record(external_exports.unknown());
      }
      return finalizePropertyZod(obj, propSchema, rootDoc, visitedPointers, defCache);
    }
    default: {
      return finalizePropertyZod(external_exports.unknown(), propSchema, rootDoc, visitedPointers, defCache);
    }
  }
}
function isEnvelopeField(propName) {
  return propName === "id" || propName === "component" || propName === "catalogId";
}
function convertPropertiesToShape(properties, requiredSet, omitEnvelopeFields = false, rootDoc, visitedPointers, defCache = /* @__PURE__ */ new Map()) {
  const shape = {};
  for (const [propName, propSchema] of Object.entries(properties)) {
    if (omitEnvelopeFields && isEnvelopeField(propName)) {
      continue;
    }
    const zodField = convertPropertyToZod(typeof propSchema === "object" && propSchema !== null ? propSchema : {}, rootDoc, visitedPointers ? new Set(visitedPointers) : /* @__PURE__ */ new Set(), defCache);
    if (!requiredSet.has(propName)) {
      shape[propName] = zodField.optional();
    } else if (zodField.safeParse(void 0).success) {
      shape[propName] = zodField.refine((value) => value !== void 0, { message: "Required" });
    } else {
      shape[propName] = zodField;
    }
  }
  return shape;
}
function collectComponentSubSchemas(schema, rootDoc, visitedPointers = /* @__PURE__ */ new Set(), isAtLeastV10 = false) {
  if (!schema || typeof schema !== "object")
    return [];
  const mixins = [];
  const local = [];
  const inline = [];
  if (Array.isArray(schema.allOf)) {
    for (const sub of schema.allOf) {
      if (!sub || typeof sub !== "object")
        continue;
      if (typeof sub.$ref === "string") {
        const ref = sub.$ref;
        if (ref.includes("common_types.json") && ref.includes("ComponentCommon")) {
          const { accessibility, metadata } = ComponentCommonSchema.shape;
          mixins.push({
            kind: "zod",
            properties: isAtLeastV10 ? { accessibility, metadata } : { accessibility }
          });
        } else if (ref.startsWith("#/")) {
          if (!visitedPointers.has(ref)) {
            visitedPointers.add(ref);
            const target = resolveJsonPointer(rootDoc, ref);
            if (target) {
              local.push(...collectComponentSubSchemas(target, rootDoc, visitedPointers, isAtLeastV10));
            }
          }
        } else {
          const mirror = resolveProtocolRef(ref);
          if (mirror instanceof external_exports.ZodObject) {
            mixins.push({ kind: "zod", properties: { ...mirror.shape } });
          }
        }
      } else {
        inline.push(...collectComponentSubSchemas(sub, rootDoc, visitedPointers, isAtLeastV10));
      }
    }
  }
  const result = [...mixins, ...local, ...inline];
  if (schema.properties) {
    result.push({ kind: "json", schema });
  }
  return result;
}
function convertComponentJsonSchemaToZod(rawSchema, rootDoc, omitEnvelopeFields = true, defCache = /* @__PURE__ */ new Map(), isAtLeastV10 = false) {
  const shape = {};
  const schemasToMerge = collectComponentSubSchemas(rawSchema, rootDoc, void 0, isAtLeastV10);
  const requiredSet = /* @__PURE__ */ new Set();
  for (const s5 of schemasToMerge) {
    if (s5.kind === "json" && Array.isArray(s5.schema.required)) {
      s5.schema.required.forEach((r8) => {
        if (typeof r8 === "string")
          requiredSet.add(r8);
      });
    }
  }
  for (const s5 of schemasToMerge) {
    if (s5.kind === "zod") {
      for (const [propName, propSchema] of Object.entries(s5.properties)) {
        if (omitEnvelopeFields && isEnvelopeField(propName)) {
          continue;
        }
        shape[propName] = propSchema;
      }
      continue;
    }
    const propShape = convertPropertiesToShape(s5.schema.properties || {}, requiredSet, omitEnvelopeFields, rootDoc, void 0, defCache);
    Object.assign(shape, propShape);
  }
  const obj = external_exports.object(shape);
  const allowExtra = rawSchema.unevaluatedProperties === true || typeof rawSchema.unevaluatedProperties === "object" && rawSchema.unevaluatedProperties !== null || rawSchema.additionalProperties === true || typeof rawSchema.additionalProperties === "object" && rawSchema.additionalProperties !== null;
  return allowExtra ? obj.passthrough() : obj.strict();
}
function isShorthandParameterMap(schema) {
  if (!schema || typeof schema !== "object" || Array.isArray(schema)) {
    return false;
  }
  const s5 = schema;
  return Object.keys(s5).length > 0 && s5.properties === void 0 && typeof s5.type !== "string" && !("$ref" in s5) && !("allOf" in s5) && !("oneOf" in s5) && !("anyOf" in s5) && !("not" in s5) && Object.values(s5).every((v3) => v3 !== null && (typeof v3 === "object" || typeof v3 === "boolean"));
}
function isExplicitlyOpen(schema) {
  return schema.unevaluatedProperties === true || typeof schema.unevaluatedProperties === "object" && schema.unevaluatedProperties !== null || schema.additionalProperties === true || typeof schema.additionalProperties === "object" && schema.additionalProperties !== null;
}
function collectArgSubSchemas(schema, rootDoc, visited = /* @__PURE__ */ new Set()) {
  const result = {
    schemas: [],
    hasUnresolvable: false
  };
  if (!schema || typeof schema !== "object" || Array.isArray(schema)) {
    return result;
  }
  if (isShorthandParameterMap(schema)) {
    result.schemas.push({
      type: "object",
      properties: schema
    });
    return result;
  }
  if (typeof schema.$ref === "string") {
    const ref = schema.$ref;
    if (ref.startsWith("#/")) {
      if (!rootDoc) {
        result.hasUnresolvable = true;
      } else if (!visited.has(ref)) {
        visited.add(ref);
        const target = resolveJsonPointer(rootDoc, ref);
        if (target) {
          const sub = collectArgSubSchemas(target, rootDoc, visited);
          result.schemas.push(...sub.schemas);
          if (sub.hasUnresolvable) {
            result.hasUnresolvable = true;
          }
        } else {
          result.hasUnresolvable = true;
        }
      }
    } else {
      result.hasUnresolvable = true;
    }
  }
  if (Array.isArray(schema.allOf)) {
    for (const sub of schema.allOf) {
      if (sub && typeof sub === "object" && !Array.isArray(sub)) {
        const subResult = collectArgSubSchemas(sub, rootDoc, visited);
        result.schemas.push(...subResult.schemas);
        if (subResult.hasUnresolvable) {
          result.hasUnresolvable = true;
        }
      }
    }
  }
  if (Array.isArray(schema.anyOf) || Array.isArray(schema.oneOf)) {
    result.hasUnresolvable = true;
  }
  if (schema.properties || schema.required || schema.additionalProperties !== void 0 || schema.unevaluatedProperties !== void 0) {
    result.schemas.push(schema);
  }
  return result;
}
function convertFunctionArgsJsonSchemaToZod(rawSchema, rootDoc, defCache = /* @__PURE__ */ new Map()) {
  const collected = collectArgSubSchemas(rawSchema, rootDoc);
  const requiredSet = /* @__PURE__ */ new Set();
  for (const s5 of collected.schemas) {
    if (Array.isArray(s5.required)) {
      for (const r8 of s5.required) {
        if (typeof r8 === "string") {
          requiredSet.add(r8);
        }
      }
    }
  }
  const shape = {};
  for (const s5 of collected.schemas) {
    if (s5.properties && typeof s5.properties === "object" && !Array.isArray(s5.properties)) {
      const propShape = convertPropertiesToShape(s5.properties, requiredSet, false, rootDoc, void 0, defCache);
      Object.assign(shape, propShape);
    }
  }
  const obj = external_exports.object(shape);
  const allowExtra = collected.hasUnresolvable || collected.schemas.some(isExplicitlyOpen) || isExplicitlyOpen(rawSchema);
  return allowExtra ? obj.passthrough() : obj.strict();
}
function functionParameterNames(schema, rootDoc) {
  if (!schema || typeof schema !== "object" || Array.isArray(schema)) {
    return [];
  }
  const s5 = schema;
  const names = [];
  const visited = /* @__PURE__ */ new Set();
  const extractProperties = (node) => {
    if (!node || typeof node !== "object" || Array.isArray(node))
      return;
    const n6 = node;
    if (typeof n6.$ref === "string" && rootDoc && n6.$ref.startsWith("#/")) {
      if (!visited.has(n6.$ref)) {
        visited.add(n6.$ref);
        const target = resolveJsonPointer(rootDoc, n6.$ref);
        if (target) {
          extractProperties(target);
        }
      }
    }
    if (n6.properties && typeof n6.properties === "object" && n6.properties !== null && !Array.isArray(n6.properties)) {
      names.push(...Object.keys(n6.properties));
    }
    if (Array.isArray(n6.allOf)) {
      for (const member of n6.allOf) {
        extractProperties(member);
      }
    }
  };
  if (s5.properties && typeof s5.properties === "object" && s5.properties !== null && !Array.isArray(s5.properties) || Array.isArray(s5.allOf) || typeof s5.$ref === "string") {
    extractProperties(s5);
    return names;
  }
  if (isShorthandParameterMap(s5)) {
    return Object.keys(s5);
  }
  return [];
}
function parseFunctionDefinitions(rawFunctions, rootDoc, permittedNames, atLeastV10 = false) {
  const result = [];
  if (!rawFunctions)
    return result;
  const defCache = /* @__PURE__ */ new Map();
  const assertFunctionIdentifiers = (name, paramNames) => {
    if (!atLeastV10)
      return;
    assertUax31Identifier(name, `function identifier: '${name}'`);
    for (const argName of paramNames) {
      assertUax31Identifier(argName, `argument identifier: '${argName}' in function '${name}'`);
    }
  };
  if (Array.isArray(rawFunctions)) {
    for (const fn of rawFunctions) {
      if (fn && typeof fn === "object" && typeof fn.name === "string") {
        const paramNames = functionParameterNames(fn.parameters, rootDoc);
        assertFunctionIdentifiers(fn.name, paramNames);
        if (permittedNames && !permittedNames.has(fn.name)) {
          continue;
        }
        result.push(new JsonFunctionApi(fn.name, fn, (definition) => definition["parameters"] && typeof definition["parameters"] === "object" ? convertFunctionArgsJsonSchemaToZod(definition["parameters"], rootDoc, defCache) : external_exports.record(external_exports.unknown())));
      }
    }
    return result;
  }
  if (typeof rawFunctions === "object" && rawFunctions !== null) {
    for (const [name, defn] of Object.entries(rawFunctions)) {
      const rawDefn = defn && typeof defn === "object" ? defn : {};
      const argsSchema = functionArgsSchema(rawDefn);
      const paramNames = functionParameterNames(argsSchema, rootDoc);
      assertFunctionIdentifiers(name, paramNames);
      if (permittedNames && !permittedNames.has(name)) {
        continue;
      }
      if (!defn || typeof defn !== "object")
        continue;
      result.push(new JsonFunctionApi(name, defn, (definition) => {
        const argsSchema2 = functionArgsSchema(definition);
        return argsSchema2 ? convertFunctionArgsJsonSchemaToZod(argsSchema2, rootDoc, defCache) : external_exports.record(external_exports.unknown());
      }));
    }
  }
  return result;
}
function functionArgsSchema(d4) {
  const props = d4.properties && typeof d4.properties === "object" && d4.properties !== null && !Array.isArray(d4.properties) ? d4.properties : void 0;
  let argsSchema = props?.args ?? d4.args ?? d4.parameters;
  if (!argsSchema && props && !("call" in props) && !("@call" in props) && !("function" in props)) {
    argsSchema = d4;
  } else if (isShorthandParameterMap(argsSchema)) {
    argsSchema = {
      type: "object",
      properties: argsSchema,
      required: d4.required,
      additionalProperties: d4.additionalProperties
    };
  }
  return argsSchema && typeof argsSchema === "object" && !Array.isArray(argsSchema) ? argsSchema : void 0;
}
function extractPermittedNames(oneOf, prefix) {
  if (!Array.isArray(oneOf))
    return void 0;
  const permitted = /* @__PURE__ */ new Set();
  for (const item of oneOf) {
    if (typeof item?.$ref === "string" && item.$ref.startsWith(prefix)) {
      const rawName = item.$ref.slice(prefix.length);
      const unescapedName = rawName.replace(/~([01])/g, (_3, p1) => p1 === "1" ? "/" : "~");
      permitted.add(unescapedName);
    }
  }
  return permitted;
}
function parseCatalogComponents(componentsMap, catalogSchema, atLeastV10, permittedNames) {
  const components = [];
  const defCache = /* @__PURE__ */ new Map();
  for (const [name, rawCompSchema] of Object.entries(componentsMap)) {
    const rawComp = rawCompSchema || {};
    if (atLeastV10) {
      assertUax31Identifier(name, `component identifier: '${name}'`);
      const props = rawComp.properties;
      if (props && typeof props === "object") {
        for (const propName of Object.keys(props)) {
          assertUax31Identifier(propName, `property identifier: '${propName}' in component '${name}'`);
        }
      }
      const rawMeta = rawComp.metadata;
      if (rawMeta !== void 0) {
        try {
          validateMetadata(rawMeta);
        } catch (err) {
          throw new A2uiCatalogError(err instanceof Error ? err.message : String(err));
        }
      }
    }
    if (permittedNames && !permittedNames.has(name)) {
      continue;
    }
    components.push(new JsonComponentApi(name, rawComp, (definition) => convertComponentJsonSchemaToZod(definition, catalogSchema, true, defCache, atLeastV10)));
  }
  return components;
}
function parseThemeSchema(catalogSchema, defs) {
  const rawTheme = catalogSchema.theme ?? catalogSchema.themeSchema ?? catalogSchema.styles ?? defs?.theme;
  if (rawTheme && typeof rawTheme === "object") {
    const rawThemeObj = rawTheme;
    const normalizedThemeSchema = "properties" in rawThemeObj || "allOf" in rawThemeObj || rawThemeObj.type === "object" ? rawThemeObj : { type: "object", properties: rawThemeObj };
    const themeZod = convertComponentJsonSchemaToZod(normalizedThemeSchema, catalogSchema, false);
    const closed = normalizedThemeSchema.additionalProperties === false || normalizedThemeSchema.unevaluatedProperties === false;
    return closed ? themeZod : themeZod.passthrough();
  }
  return void 0;
}
function isSupportedV10Ref(ref) {
  if (ref.startsWith("#"))
    return true;
  const hashIndex = ref.indexOf("#");
  const documentPart = hashIndex === -1 ? ref : ref.slice(0, hashIndex);
  if (documentPart === "catalog.json")
    return true;
  return documentPart.split("/").pop() === "common_types.json";
}
function escapeJsonPointerToken(token) {
  return token.replace(/~/g, "~0").replace(/\//g, "~1");
}
function assertSupportedV10Refs(node, pointer) {
  if (Array.isArray(node)) {
    node.forEach((item, index) => assertSupportedV10Refs(item, `${pointer}/${index}`));
    return;
  }
  if (!isPlainObject3(node))
    return;
  for (const [key, value] of Object.entries(node)) {
    const childPointer = `${pointer}/${escapeJsonPointerToken(key)}`;
    if (key === "$ref" && typeof value === "string" && !isSupportedV10Ref(value)) {
      throw new A2uiCatalogError(`Unsupported external $ref '${value}' at ${childPointer}: a v1.0 catalog may only reference its own components and functions (#/...) or common_types.json.`);
    }
    assertSupportedV10Refs(value, childPointer);
  }
}
function parseCatalogSchema(inputSchema, protocolVersion, catalogIdOverride) {
  const catalogSchema = deepCopyJson(inputSchema);
  const catalogId = catalogIdOverride ?? catalogSchema.catalogId ?? catalogSchema.$id ?? catalogSchema.id;
  if (!catalogId || typeof catalogId !== "string") {
    throw new Error("Catalog ID must be specified via catalog metadata ('catalogId' or '$id').");
  }
  const defs = catalogSchema.$defs;
  const anyComp = defs?.anyComponent;
  const permittedNames = extractPermittedNames(anyComp?.oneOf, "#/components/");
  const resolvedVersion = protocolVersion ?? catalogSchema.protocolVersion ?? DEFAULT_PROTOCOL_VERSION;
  const atLeastV10 = isAtLeastVersion(resolvedVersion, "1.0");
  if (atLeastV10) {
    assertSupportedV10Refs(catalogSchema, "");
  }
  const componentsMap = catalogSchema.components ?? {};
  const components = parseCatalogComponents(componentsMap, catalogSchema, atLeastV10, permittedNames);
  const anyFunc = defs?.anyFunction;
  const permittedFunctionNames = extractPermittedNames(anyFunc?.oneOf, "#/functions/");
  const functions = parseFunctionDefinitions(catalogSchema.functions, catalogSchema, permittedFunctionNames, atLeastV10);
  const themeSchema = parseThemeSchema(catalogSchema, defs);
  deepFreeze(catalogSchema);
  return {
    id: catalogId,
    protocolVersion: resolvedVersion,
    components,
    functions,
    themeSchema,
    ...readDocumentFields(catalogSchema, components, functions)
  };
}
var DOCUMENT_STRING_FIELDS = {
  "$schema": "schemaUri",
  "$id": "schemaId",
  "title": "title",
  "description": "description",
  "instructions": "instructions"
};
var DOCUMENT_STRUCTURE_FIELDS = /* @__PURE__ */ new Set(["protocolVersion", "catalogId", "components"]);
function isPlainObject3(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
function isComputedUnion(actual, computed2) {
  return canonicalJsonStringify(actual) === canonicalJsonStringify(computed2);
}
function readDocumentFields(document2, components, functions) {
  const fields = {};
  const extensionFields = {};
  for (const [key, value] of Object.entries(document2)) {
    if (key in DOCUMENT_STRING_FIELDS && typeof value === "string") {
      fields[DOCUMENT_STRING_FIELDS[key]] = value;
    } else if (key === "protocolVersion" && typeof value === "string") {
      fields.declaredProtocolVersion = value;
    } else if (key === "functions" && (Array.isArray(value) || isPlainObject3(value))) {
      fields.functionsLayout = Array.isArray(value) ? "list" : "map";
    } else if (key === "$defs" && isPlainObject3(value)) {
      const anyComponent = isComputedUnion(value["anyComponent"], buildComponentUnion(components.map((c5) => c5.name)));
      const anyFunction = isComputedUnion(value["anyFunction"], buildFunctionUnion(functions.map((f4) => f4.name)));
      fields.unions = { anyComponent, anyFunction };
      const defs = {};
      for (const [name, def] of Object.entries(value)) {
        if (name === "anyComponent" && anyComponent || name === "anyFunction" && anyFunction) {
          continue;
        }
        defs[name] = def;
      }
      fields.defs = Object.freeze(defs);
    } else if (!DOCUMENT_STRUCTURE_FIELDS.has(key) || key === "components" && !isPlainObject3(value)) {
      extensionFields[key] = value;
    }
  }
  fields.declaredProtocolVersion ??= null;
  fields.declaresCatalogId = typeof document2["catalogId"] === "string";
  fields.declaresComponents = isPlainObject3(document2["components"]);
  fields.unions ??= { anyComponent: false, anyFunction: false };
  if (Object.keys(extensionFields).length > 0) {
    fields.extensionFields = Object.freeze(extensionFields);
  }
  return fields;
}

// node_modules/@a2ui/web_core/src/catalog/schema_generator.js
function getStandardDefsForCatalog(catalog, options) {
  if (options?.standardDefs) {
    return options.standardDefs;
  }
  return standardDefsForVersion(options?.protocolVersion ?? catalog.protocolVersion);
}
var EXTERNAL_DEF_REF = /^[^#]+#\/(?:\$defs|definitions)\/([^/]+)(\/.*)?$/;
function localizeExternalDefRef(ref) {
  const match = EXTERNAL_DEF_REF.exec(ref);
  if (match)
    return `#/$defs/${match[1]}${match[2] ?? ""}`;
  return `#/$defs/${ref.split("/").pop()}`;
}
function localDefName(ref) {
  if (!ref.startsWith("#/$defs/"))
    return void 0;
  return ref.substring("#/$defs/".length).split("/")[0].replace(/~1/g, "/").replace(/~0/g, "~");
}
function transformRefDescriptionNode(obj) {
  if (typeof obj.description !== "string" || !obj.description.startsWith("REF:")) {
    return false;
  }
  const content = obj.description.substring(4);
  const pipeIndex = content.indexOf("|");
  const ref = pipeIndex === -1 ? content : content.substring(0, pipeIndex);
  const desc = pipeIndex === -1 ? "" : content.substring(pipeIndex + 1);
  const savedDefault = obj.default;
  for (const key of Object.keys(obj)) {
    delete obj[key];
  }
  obj["$ref"] = ref.startsWith("#") ? ref : localizeExternalDefRef(ref);
  if (savedDefault !== void 0) {
    obj["default"] = savedDefault;
  }
  if (desc) {
    obj["description"] = desc;
  }
  return true;
}
function cleanSchemaProperties(obj, options = {}) {
  if (Array.isArray(obj.allOf)) {
    const members = obj.allOf.filter((m4) => !(typeof m4 === "object" && m4 !== null && Object.keys(m4).length === 0));
    const only = members.length === 1 ? members[0] : void 0;
    if (typeof only === "object" && only !== null && !Array.isArray(only) && Object.keys(only).every((key) => !(key in obj) || key === "allOf")) {
      delete obj.allOf;
      Object.assign(obj, only);
    } else if (members.length === 0) {
      delete obj.allOf;
    } else {
      obj.allOf = members;
    }
  }
  if (options.stripAdditionalProperties) {
    delete obj["additionalProperties"];
    delete obj["unevaluatedProperties"];
  } else {
    if (obj.additionalProperties && typeof obj.additionalProperties === "object" && Object.keys(obj.additionalProperties).length === 0) {
      obj.additionalProperties = true;
    }
    if (obj.unevaluatedProperties && typeof obj.unevaluatedProperties === "object" && Object.keys(obj.unevaluatedProperties).length === 0) {
      obj.unevaluatedProperties = true;
    }
  }
  if ("$schema" in obj) {
    delete obj["$schema"];
  }
}
function cleanSchemaNode(node, visited = /* @__PURE__ */ new Set(), options = {}) {
  if (typeof node !== "object" || node === null)
    return;
  if (visited.has(node))
    return;
  visited.add(node);
  if (Array.isArray(node)) {
    for (const item of node) {
      cleanSchemaNode(item, visited, options);
    }
    return;
  }
  const obj = node;
  if (transformRefDescriptionNode(obj)) {
    return;
  }
  cleanSchemaProperties(obj, options);
  for (const key of Object.keys(obj)) {
    cleanSchemaNode(obj[key], visited, options);
  }
}
function isJsonObject(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
var REF_ANNOTATION_KEYWORDS = /* @__PURE__ */ new Set([
  "description",
  "title",
  "default",
  "deprecated",
  "readOnly",
  "writeOnly",
  "examples"
]);
var COMPONENT_ENVELOPE_KEYS = /* @__PURE__ */ new Set(["id", "component", "catalogId"]);
var GENERATED_DEF_NAMES = /* @__PURE__ */ new Set(["anyComponent", "anyFunction", "theme"]);
var COMMON_TYPE_REF = /^(?:([^#]*common_types\.json))?#\/\$defs\/([^/]+)(\/.*)?$/;
var STANDARD_MIXIN_REF = /\/(ComponentCommon|Checkable)$/;
function localizeStandardRef(node, name, standardDefs, pointer = "") {
  node["$ref"] = `#/$defs/${name}${pointer}`;
  if (pointer || node["description"] !== void 0)
    return;
  const definition = standardDefs[name];
  if (isJsonObject(definition) && typeof definition["description"] === "string") {
    node["description"] = definition["description"];
  }
}
function restoreSourceRefs(node, standardDefs, annotationsOnly) {
  if (Array.isArray(node)) {
    for (const item of node)
      restoreSourceRefs(item, standardDefs, annotationsOnly);
    return;
  }
  if (!isJsonObject(node))
    return;
  const ref = node["$ref"];
  const match = typeof ref === "string" ? COMMON_TYPE_REF.exec(ref) : null;
  if (match) {
    const [, document2, name, pointer] = match;
    if (document2 !== void 0 || name in standardDefs) {
      if (annotationsOnly) {
        for (const key of Object.keys(node)) {
          if (key !== "$ref" && !REF_ANNOTATION_KEYWORDS.has(key))
            delete node[key];
        }
        localizeStandardRef(node, name, standardDefs, pointer);
        return;
      }
      localizeStandardRef(node, name, standardDefs, pointer);
    }
  }
  for (const value of Object.values(node)) {
    restoreSourceRefs(value, standardDefs, annotationsOnly);
  }
}
function resolveLocalPointer(rootDoc, pointer) {
  if (!pointer.startsWith("#/"))
    return void 0;
  let current = rootDoc;
  for (const raw of pointer.substring(2).split("/")) {
    const segment = raw.replace(/~([01])/g, (_3, p1) => p1 === "1" ? "/" : "~");
    if (!isJsonObject(current) || !(segment in current))
      return void 0;
    current = current[segment];
  }
  return current;
}
function collectSourceSubSchemas(schema, context, visited) {
  const inline = [];
  const local = [];
  const mixins = [];
  for (const sub of Array.isArray(schema["allOf"]) ? schema["allOf"] : []) {
    if (!isJsonObject(sub))
      continue;
    const ref = sub["$ref"];
    if (typeof ref !== "string") {
      inline.push(...collectSourceSubSchemas(sub, context, visited));
      continue;
    }
    const mixinName = STANDARD_MIXIN_REF.exec(ref)?.[1];
    const target = ref.startsWith("#/") ? resolveLocalPointer(context.rootDoc, ref) : void 0;
    if (mixinName === void 0 && isJsonObject(target)) {
      if (!visited.has(ref)) {
        visited.add(ref);
        local.push(...collectSourceSubSchemas(target, context, visited));
      }
      continue;
    }
    const external = EXTERNAL_DEF_REF.exec(ref);
    const name = mixinName ?? (external && !external[2] ? external[1] : void 0);
    const mixin = name !== void 0 ? context.standardDefs[name] : void 0;
    if (isJsonObject(mixin) && isJsonObject(mixin["properties"])) {
      mixins.push({
        properties: deepCopyJson(mixin["properties"]),
        ...Array.isArray(mixin["required"]) ? { required: mixin["required"] } : {}
      });
    }
  }
  const pieces = [...mixins, ...local, ...inline];
  if ("properties" in schema || "description" in schema)
    pieces.push(schema);
  return pieces;
}
function sourceComponentSchema(source, context) {
  const pieces = collectSourceSubSchemas(source, context, /* @__PURE__ */ new Set());
  let description;
  const props = {};
  const required = /* @__PURE__ */ new Set();
  for (const piece of pieces) {
    if (typeof piece["description"] === "string")
      description = piece["description"];
    if (isJsonObject(piece["properties"]))
      Object.assign(props, piece["properties"]);
    if (Array.isArray(piece["required"])) {
      for (const name of piece["required"]) {
        if (typeof name === "string")
          required.add(name);
      }
    }
  }
  const copied = {};
  for (const [name, schema] of Object.entries(props)) {
    if (COMPONENT_ENVELOPE_KEYS.has(name))
      continue;
    const copy = deepCopyJson(schema);
    restoreSourceRefs(copy, context.standardDefs, true);
    copied[name] = copy;
  }
  return {
    props: copied,
    required: [...required].filter((name) => !COMPONENT_ENVELOPE_KEYS.has(name)).sort(),
    ...description !== void 0 ? { description } : {}
  };
}
function authoredFunctionArgs(name, context) {
  const source = context.model.functions.get(name);
  if (!source)
    return void 0;
  const props = isJsonObject(source.properties) ? source.properties : void 0;
  const explicit = props?.args ?? source.args ?? source.parameters;
  if (explicit !== void 0) {
    if (!isJsonObject(explicit))
      return {};
    if (isShorthandParameterMap(explicit)) {
      return {
        type: "object",
        properties: explicit,
        ...source.required !== void 0 ? { required: source.required } : {},
        ...source.additionalProperties !== void 0 ? { additionalProperties: source.additionalProperties } : {}
      };
    }
    return explicit;
  }
  const isCallSchema = props && ("call" in props || "@call" in props || "function" in props);
  if (!props || isCallSchema)
    return {};
  const { description: _description, title: _title, name: _name, returnType: _returnType, allowedCallers: _allowedCallers, requiresUserActivation: _requiresUserActivation, ...args } = source;
  return args;
}
function authoredTheme(model) {
  const theme = THEME_DOCUMENT_KEYS.map((key) => model.extensionFields[key]).find((value) => value !== void 0) ?? model.defs["theme"];
  if (!isJsonObject(theme))
    return void 0;
  return "properties" in theme || "allOf" in theme || theme.type === "object" ? theme : { type: "object", properties: theme };
}
function isV08Catalog(catalog) {
  const version = toCanonicalVersion(catalog.protocolVersion) ?? catalog.protocolVersion;
  return version === "0.8" || catalog.id.includes("v0_8") || catalog.id.includes("v0.8");
}
function processTheme(catalog, defs, generated, context) {
  if (!catalog.themeSchema)
    return;
  const isV08 = isV08Catalog(catalog);
  const authored = authoredTheme(context.model);
  if (authored) {
    const theme = deepCopyJson({ ...authored });
    if (!isV08 && theme["additionalProperties"] === void 0 && theme["unevaluatedProperties"] === void 0) {
      theme["additionalProperties"] = true;
    }
    defs["theme"] = theme;
    return;
  }
  const themeRaw = zodToJsonSchema(catalog.themeSchema, {
    target: "jsonSchema2019-09",
    $refStrategy: "none",
    override: exclusiveUnionOverride
  });
  cleanSchemaNode(themeRaw);
  mergeGeneratedDefs(themeRaw, defs, generated);
  const closed = themeRaw.additionalProperties === false;
  const themeObj = {
    type: "object",
    ...typeof themeRaw.description === "string" && !themeRaw.description.startsWith("REF:") ? { description: themeRaw.description } : {},
    properties: themeRaw.properties || {},
    ...Array.isArray(themeRaw.required) && themeRaw.required.length > 0 ? { required: themeRaw.required } : {},
    ...closed ? { additionalProperties: false } : isV08 ? {} : {
      additionalProperties: themeRaw.additionalProperties !== void 0 ? themeRaw.additionalProperties : true
    }
  };
  generated.push(themeObj);
  defs["theme"] = themeObj;
}
function mergeGeneratedDefs(raw, defs, generated) {
  const nested = isJsonObject(raw.definitions) ? raw.definitions : isJsonObject(raw.$defs) ? raw.$defs : void 0;
  delete raw.definitions;
  delete raw.$defs;
  if (!nested)
    return;
  Object.assign(defs, nested);
  generated.push(nested);
}
function extractZodComponentSchema(comp, defs, generated) {
  if (!comp.schema || typeof comp.schema !== "object" || !("safeParse" in comp.schema)) {
    return { props: {}, reqList: [], additionalProps: void 0 };
  }
  const rawZod = zodToJsonSchema(comp.schema, {
    target: "jsonSchema2019-09",
    $refStrategy: "none",
    override: exclusiveUnionOverride
  });
  cleanSchemaNode(rawZod);
  mergeGeneratedDefs(rawZod, defs, generated);
  const props = rawZod.properties || {};
  const reqList = Array.isArray(rawZod.required) ? rawZod.required.filter((r8) => r8 !== "component" && r8 !== "id") : [];
  const rawExtra = rawZod.unevaluatedProperties ?? rawZod.additionalProperties;
  const additionalProps = typeof rawExtra === "boolean" || isJsonObject(rawExtra) ? rawExtra : void 0;
  return { props, reqList, additionalProps };
}
function zodComponentDescription(comp) {
  const zodDescription = comp.schema?.description;
  return typeof zodDescription === "string" && !zodDescription.startsWith("REF:") ? zodDescription : void 0;
}
function authoredClosure(source) {
  return source["unevaluatedProperties"] ?? source["additionalProperties"];
}
function processSingleComponent(name, comp, defs, generated, context, options) {
  let props;
  let reqList;
  let description;
  let closure;
  const source = context.model.components.get(name);
  if (source) {
    ({ props, required: reqList, description } = sourceComponentSchema(source, context));
    closure = authoredClosure(source) ?? (context.isV10 ? void 0 : false);
  } else {
    const extracted = extractZodComponentSchema(comp, defs, generated);
    const { component: _ignoredComp, id: _ignoredId, ...sanitized } = extracted.props;
    generated.push(sanitized);
    props = sanitized;
    reqList = extracted.reqList;
    description = zodComponentDescription(comp);
    closure = context.isV10 ? void 0 : extracted.additionalProps ?? false;
  }
  const id = {};
  localizeStandardRef(id, "ComponentId", context.standardDefs);
  const innerProperties = { id, ...props, component: { const: name } };
  const innerRequired = ["id", ...reqList, "component"];
  const closureEntry = closure !== void 0 ? { unevaluatedProperties: closure } : {};
  let compSchemaObj;
  if (options?.componentEnvelopeRef) {
    compSchemaObj = {
      ...description !== void 0 ? { description } : {},
      allOf: [
        { $ref: options.componentEnvelopeRef },
        {
          type: "object",
          properties: innerProperties,
          required: innerRequired
        }
      ],
      ...closureEntry
    };
  } else {
    compSchemaObj = {
      type: "object",
      ...description !== void 0 ? { description } : {},
      properties: innerProperties,
      required: innerRequired,
      ...closureEntry
    };
  }
  if (comp.allowedParents && comp.allowedParents.length > 0) {
    compSchemaObj["allowedParents"] = comp.allowedParents;
  }
  if (comp.allowedChildren && comp.allowedChildren.length > 0) {
    compSchemaObj["allowedChildren"] = comp.allowedChildren;
  }
  return compSchemaObj;
}
function processComponents(catalog, schema, defs, generated, context, options) {
  if (catalog.components.size === 0) {
    schema["components"] = {};
    return;
  }
  const componentsMap = {};
  for (const [name, comp] of catalog.components.entries()) {
    componentsMap[name] = processSingleComponent(name, comp, defs, generated, context, options);
  }
  schema["components"] = componentsMap;
  defs["anyComponent"] = {
    oneOf: Array.from(catalog.components.keys()).map((name) => ({
      $ref: `#/components/${name}`
    })),
    discriminator: {
      propertyName: "component"
    }
  };
}
function buildFunctionArgsSchema(name, fn, defs, generated, context) {
  let paramSchemaObj;
  const authored = authoredFunctionArgs(name, context);
  if (authored) {
    paramSchemaObj = deepCopyJson(authored);
    restoreSourceRefs(paramSchemaObj, context.standardDefs, false);
  } else if (fn.schema && typeof fn.schema === "object" && "safeParse" in fn.schema) {
    const rawZod = zodToJsonSchema(fn.schema, {
      target: "jsonSchema2019-09",
      $refStrategy: "none",
      override: exclusiveUnionOverride
    });
    cleanSchemaNode(rawZod);
    mergeGeneratedDefs(rawZod, defs, generated);
    generated.push(rawZod);
    paramSchemaObj = rawZod;
  } else {
    paramSchemaObj = { type: "object", properties: {} };
  }
  if (paramSchemaObj.type === "object") {
    const additional = paramSchemaObj.additionalProperties;
    delete paramSchemaObj.additionalProperties;
    paramSchemaObj.unevaluatedProperties = paramSchemaObj.unevaluatedProperties ?? additional ?? false;
  }
  return paramSchemaObj;
}
function processSingleFunction(name, fn, defs, generated, context) {
  const argsSchema = buildFunctionArgsSchema(name, fn, defs, generated, context);
  const hasRequiredArgs = schemaRequiresArgs(argsSchema, { ...context.defs, ...defs });
  const callKey = context.isV10 ? "@call" : "call";
  const required = hasRequiredArgs ? [callKey, "args"] : [callKey];
  if (context.isV10) {
    return {
      type: "object",
      ...fn.description ? { description: fn.description } : {},
      returnType: fn.returnType,
      ...fn.allowedCallers !== void 0 ? { allowedCallers: fn.allowedCallers } : {},
      ...fn.requiresUserActivation !== void 0 ? { requiresUserActivation: fn.requiresUserActivation } : {},
      properties: {
        "@call": { const: name },
        args: argsSchema
      },
      required
    };
  }
  return {
    type: "object",
    ...fn.description ? { description: fn.description } : {},
    properties: {
      call: { const: name },
      args: argsSchema,
      returnType: { const: fn.returnType }
    },
    required,
    unevaluatedProperties: false
  };
}
function processFunctions(catalog, schema, defs, generated, context) {
  const names = Array.from(catalog.functions.keys()).filter((name) => !name.startsWith("@"));
  if (names.length === 0)
    return;
  const functionsMap = {};
  for (const name of names) {
    functionsMap[name] = processSingleFunction(name, catalog.functions.get(name), defs, generated, context);
  }
  schema["functions"] = functionsMap;
  defs["anyFunction"] = {
    oneOf: names.map((name) => ({
      $ref: `#/functions/${name}`
    }))
  };
}
var MIRROR_REF_DESCRIPTIONS = new Map(Object.values(CommonSchemas).flatMap((mirror) => {
  const description = mirror.description;
  if (typeof description !== "string" || !description.startsWith("REF:"))
    return [];
  const content = description.substring(4);
  const pipeIndex = content.indexOf("|");
  if (pipeIndex === -1)
    return [];
  const name = content.substring(0, pipeIndex).split("/").pop();
  return [[name, content.substring(pipeIndex + 1)]];
}));
function localizeRefs(node, keep, visited = /* @__PURE__ */ new Set()) {
  if (typeof node !== "object" || node === null || visited.has(node))
    return;
  visited.add(node);
  if (Array.isArray(node)) {
    for (const item of node)
      localizeRefs(item, keep, visited);
    return;
  }
  const obj = node;
  const ref = obj["$ref"];
  if (typeof ref === "string" && !ref.startsWith("#") && ref !== keep) {
    if (!EXTERNAL_DEF_REF.test(ref)) {
      throw new Error(`Cannot bundle the external schema reference '${ref}'.`);
    }
    obj["$ref"] = localizeExternalDefRef(ref);
  }
  for (const value of Object.values(obj))
    localizeRefs(value, keep, visited);
}
function describeStandardRefs(node, standardDefs, visited = /* @__PURE__ */ new Set()) {
  if (typeof node !== "object" || node === null || visited.has(node))
    return;
  visited.add(node);
  if (Array.isArray(node)) {
    for (const item of node)
      describeStandardRefs(item, standardDefs, visited);
    return;
  }
  const obj = node;
  const ref = obj["$ref"];
  if (typeof ref === "string" && ref.startsWith("#/$defs/")) {
    const name = ref.substring("#/$defs/".length);
    const definition = name.includes("/") ? void 0 : standardDefs[name];
    const inherited = obj["description"] === void 0 || obj["description"] === MIRROR_REF_DESCRIPTIONS.get(name);
    if (definition && inherited) {
      if (typeof definition["description"] === "string") {
        obj["description"] = definition["description"];
      } else {
        delete obj["description"];
      }
    }
  }
  for (const value of Object.values(obj))
    describeStandardRefs(value, standardDefs, visited);
}
function collectReferencedDefs(node, referenced, visited = /* @__PURE__ */ new Set()) {
  if (typeof node !== "object" || node === null)
    return;
  if (visited.has(node))
    return;
  visited.add(node);
  if (typeof node["$ref"] === "string") {
    const name = localDefName(node["$ref"]);
    if (name !== void 0)
      referenced.add(name);
  }
  if (Array.isArray(node)) {
    for (const item of node) {
      collectReferencedDefs(item, referenced, visited);
    }
  } else {
    for (const val of Object.values(node)) {
      collectReferencedDefs(val, referenced, visited);
    }
  }
}
function generateValidationSchema(catalog, options) {
  const schema = {
    $schema: "https://json-schema.org/draft/2020-12/schema",
    // The declared version, in the bare semantic version form
    // catalog_definition.json requires: `v0.9.1` becomes `0.9.1`.
    ...typeof catalog.declaredProtocolVersion === "string" ? { protocolVersion: catalog.declaredProtocolVersion.trim().replace(/^[vV](?=\d)/, "") } : {},
    catalogId: catalog.id
  };
  if (catalog.instructions) {
    schema["instructions"] = catalog.instructions;
  }
  const standardDefs = getStandardDefsForCatalog(catalog, options);
  const version = options?.protocolVersion ?? catalog.protocolVersion;
  const model = catalogDocumentModel(catalog);
  const authoredDefs = model.defs;
  const context = {
    standardDefs,
    model,
    defs: authoredDefs,
    rootDoc: { $defs: authoredDefs },
    isV10: Boolean(version) && isAtLeastVersion(version, "1.0")
  };
  const defs = {};
  const generated = [];
  processTheme(catalog, defs, generated, context);
  processComponents(catalog, schema, defs, generated, context, options);
  processFunctions(catalog, schema, defs, generated, context);
  localizeRefs(schema, options?.componentEnvelopeRef);
  localizeRefs(defs);
  for (const node of generated)
    describeStandardRefs(node, standardDefs);
  let defsCountBefore;
  do {
    defsCountBefore = Object.keys(defs).length;
    const referenced = /* @__PURE__ */ new Set();
    collectReferencedDefs(schema, referenced);
    collectReferencedDefs(defs, referenced);
    if (referenced.has("DynamicString") || referenced.has("DynamicNumber") || referenced.has("DynamicBoolean") || referenced.has("DynamicValue")) {
      referenced.add("DataBinding");
      referenced.add("FunctionCall");
    }
    for (const refName of referenced) {
      if (refName in defs)
        continue;
      if (refName in standardDefs) {
        const copied = deepCopyJson(standardDefs[refName]);
        localizeRefs(copied);
        defs[refName] = copied;
      } else if (!GENERATED_DEF_NAMES.has(refName) && refName in authoredDefs) {
        const copied = deepCopyJson(authoredDefs[refName]);
        restoreSourceRefs(copied, standardDefs, false);
        localizeRefs(copied);
        defs[refName] = copied;
      }
    }
    if (referenced.has("anyFunction") && !("anyFunction" in defs)) {
      defs["anyFunction"] = { not: {} };
    }
  } while (Object.keys(defs).length > defsCountBefore);
  if (Object.keys(defs).length > 0) {
    schema["$defs"] = defs;
  }
  return schema;
}

// node_modules/@a2ui/web_core/src/catalog/reference-map.js
function extractRefDefName(ref) {
  if (!ref)
    return "";
  const hashIdx = ref.lastIndexOf("#");
  const target = hashIdx !== -1 ? ref.slice(hashIdx + 1) : ref;
  const segments = target.split("/").filter(Boolean);
  return segments.pop() ?? "";
}
function unwrapNextZodLayer(current, visited) {
  if (!current?._def)
    return null;
  if (visited) {
    if (visited.has(current))
      return null;
    visited.add(current);
  }
  const typeName = current._def.typeName;
  switch (typeName) {
    case "ZodOptional":
    case "ZodNullable":
    case "ZodDefault":
    case "ZodReadonly":
      return current._def.innerType;
    case "ZodBranded":
      return current._def.type;
    case "ZodEffects":
      return current._def.schema;
    case "ZodLazy":
      return current._def.getter();
    default:
      return null;
  }
}
function getDescriptions(type) {
  const descriptions = [];
  const visited = /* @__PURE__ */ new Set();
  let current = type;
  while (current) {
    if (current.description)
      descriptions.push(current.description);
    if (current._def?.description)
      descriptions.push(current._def.description);
    const next = unwrapNextZodLayer(current, visited);
    if (!next)
      break;
    current = next;
  }
  return descriptions;
}
function unwrapZodType(type) {
  const visited = /* @__PURE__ */ new Set();
  let current = type;
  while (current) {
    const next = unwrapNextZodLayer(current, visited);
    if (!next)
      break;
    current = next;
  }
  return current;
}
function checkDirectJsonRef(ref, options) {
  if (!ref)
    return null;
  const defName = extractRefDefName(ref);
  const childListNames = options.childListRefNames;
  const childNames = options.childRefNames;
  if (childListNames.has(defName)) {
    return { isChild: false, isChildList: true };
  }
  if (childNames.has(defName)) {
    return { isChild: true, isChildList: false };
  }
  return null;
}
function checkCombinerList(list, options) {
  let hasChild = false;
  for (const sub of list) {
    if (typeof sub !== "object" || sub === null)
      continue;
    const subRes = checkJsonSchemaRef(sub, options);
    if (subRes.isChildList)
      return { isChild: false, isChildList: true };
    if (subRes.isChild)
      hasChild = true;
  }
  return hasChild ? { isChild: true, isChildList: false } : null;
}
function checkCombinerRefs(schema, options) {
  let hasChild = false;
  for (const combiner of ["oneOf", "anyOf", "allOf"]) {
    const list = schema[combiner];
    if (!Array.isArray(list))
      continue;
    const res = checkCombinerList(list, options);
    if (res?.isChildList)
      return res;
    if (res?.isChild)
      hasChild = true;
  }
  return hasChild ? { isChild: true, isChildList: false } : null;
}
function checkJsonSchemaRef(schema, options) {
  const ref = typeof schema.$ref === "string" ? schema.$ref : "";
  const directMatch = checkDirectJsonRef(ref, options);
  if (directMatch)
    return directMatch;
  if (schema.type === "array" && schema.items) {
    const itemsRes = checkJsonSchemaRef(schema.items, options);
    if (itemsRes.isChild || itemsRes.isChildList) {
      return { isChild: false, isChildList: true };
    }
  }
  const combinerMatch = checkCombinerRefs(schema, options);
  if (combinerMatch)
    return combinerMatch;
  if (schema.type === "object" && schema.properties && ("componentId" in schema.properties && ("path" in schema.properties || "dataBinding" in schema.properties) || "explicitList" in schema.properties || "template" in schema.properties)) {
    return { isChild: false, isChildList: true };
  }
  return { isChild: false, isChildList: false };
}
function isTemplateShape(unwrappedOpt) {
  if (unwrappedOpt?._def?.typeName === "ZodObject" && typeof unwrappedOpt._def.shape === "function") {
    const shape = unwrappedOpt._def.shape();
    return Boolean(shape.componentId && (shape.path || shape.dataBinding) || shape.explicitList || shape.template);
  }
  return false;
}
function isArrayOfChild(unwrappedOpt, options) {
  if (unwrappedOpt?._def?.typeName === "ZodArray") {
    const elem = unwrapZodType(unwrappedOpt._def.type);
    return analyzeChildRefSchema(elem, options).isChild;
  }
  return false;
}
function analyzeUnionOptions(options, analysisOptions) {
  let hasTemplate = false;
  let hasArrayOfChild = false;
  let hasChildRef = false;
  for (const opt of options) {
    const unwrappedOpt = unwrapZodType(opt);
    if (isTemplateShape(unwrappedOpt)) {
      hasTemplate = true;
    }
    if (isArrayOfChild(unwrappedOpt, analysisOptions)) {
      hasArrayOfChild = true;
    }
    const optRes = analyzeChildRefSchema(unwrappedOpt, analysisOptions);
    if (optRes.isChildList) {
      return { isChild: false, isChildList: true };
    }
    if (optRes.isChild) {
      hasChildRef = true;
    }
  }
  if (hasTemplate || hasArrayOfChild) {
    return { isChild: false, isChildList: true };
  }
  if (hasChildRef) {
    return { isChild: true, isChildList: false };
  }
  return { isChild: false, isChildList: false };
}
function isChildListDescription(desc, childListNames) {
  if (!desc)
    return false;
  if (desc.startsWith("REF:")) {
    const refTarget2 = extractRefDefName(desc.substring(4).split("|")[0]);
    if (childListNames.has(refTarget2))
      return true;
  }
  const refTarget = extractRefDefName(desc.split("|")[0]);
  if (refTarget && childListNames.has(refTarget)) {
    return true;
  }
  const trimmed = desc.trim();
  if (childListNames.has(trimmed)) {
    return true;
  }
  return false;
}
function isChildDescription(desc, childNames) {
  if (!desc)
    return false;
  if (desc.startsWith("REF:")) {
    const refTarget2 = extractRefDefName(desc.substring(4).split("|")[0]);
    if (childNames.has(refTarget2))
      return true;
  }
  const refTarget = extractRefDefName(desc.split("|")[0]);
  if (refTarget && childNames.has(refTarget)) {
    return true;
  }
  const trimmed = desc.trim();
  if (childNames.has(trimmed)) {
    return true;
  }
  return false;
}
function checkDescriptionChildRef(descriptions, options) {
  const childListNames = options.childListRefNames;
  const childNames = options.childRefNames;
  for (const desc of descriptions) {
    if (isChildListDescription(desc, childListNames)) {
      return { isChild: false, isChildList: true };
    }
    if (isChildDescription(desc, childNames)) {
      return { isChild: true, isChildList: false };
    }
  }
  return null;
}
function analyzeZodArray(elem, options) {
  const elemRes = analyzeChildRefSchema(elem, options);
  if (elemRes.isChild || elemRes.isChildList) {
    return { isChild: false, isChildList: true };
  }
  return { isChild: false, isChildList: false };
}
function analyzeChildRefSchema(schema, options) {
  if (!schema || typeof schema !== "object") {
    return { isChild: false, isChildList: false };
  }
  const directKind = childRefKindOf(schema);
  if (directKind === "component-id") {
    return { isChild: true, isChildList: false };
  }
  if (directKind === "child-list") {
    return { isChild: false, isChildList: true };
  }
  const descMatch = checkDescriptionChildRef(getDescriptions(schema), options);
  if (descMatch)
    return descMatch;
  const current = unwrapZodType(schema);
  if (!current?._def) {
    return checkJsonSchemaRef(schema, options);
  }
  const unwrappedKind = childRefKindOf(current);
  if (unwrappedKind === "component-id") {
    return { isChild: true, isChildList: false };
  }
  if (unwrappedKind === "child-list") {
    return { isChild: false, isChildList: true };
  }
  const typeName = current._def.typeName;
  if (typeName === "ZodArray") {
    const elem = unwrapZodType(current._def.type);
    return analyzeZodArray(elem, options);
  }
  if (typeName === "ZodUnion") {
    const unionOptions = current._def.options ?? [];
    return analyzeUnionOptions(unionOptions, options);
  }
  if (typeName === "ZodObject" && typeof current._def.shape === "function") {
    const shape = current._def.shape();
    if (shape.componentId && (shape.path || shape.dataBinding) || shape.explicitList || shape.template) {
      return { isChild: false, isChildList: true };
    }
  }
  return { isChild: false, isChildList: false };
}
function addNestedRef(acc, key, subKey) {
  const existing = acc.nestedRefs[key];
  if (existing) {
    existing.add(subKey);
    return;
  }
  acc.nestedRefs[key] = /* @__PURE__ */ new Set([subKey]);
}
function inspectShapeField(key, fieldSchema, acc, options) {
  const res = analyzeChildRefSchema(fieldSchema, options);
  if (res.isChildList) {
    acc.listRefs.add(key);
    return;
  }
  if (res.isChild) {
    acc.singleRefs.add(key);
    return;
  }
  const inner = unwrapZodType(fieldSchema);
  if (options.childPropertyNames?.has(key) && inner?._def?.typeName === "ZodString") {
    acc.singleRefs.add(key);
    return;
  }
  if (inner?._def?.typeName !== "ZodArray")
    return;
  const elem = unwrapZodType(inner._def.type);
  if (elem?._def?.typeName !== "ZodObject" || typeof elem._def.shape !== "function")
    return;
  const elemShape = elem._def.shape();
  for (const [subKey, subSchema] of Object.entries(elemShape)) {
    const subRes = analyzeChildRefSchema(subSchema, options);
    const subInner = unwrapZodType(subSchema);
    if (subRes.isChild || subRes.isChildList || options.childPropertyNames?.has(subKey) && subInner?._def?.typeName === "ZodString") {
      acc.listRefs.add(key);
      addNestedRef(acc, key, subKey);
    }
  }
}
function rawArrayItemProperties(propSchema) {
  if (typeof propSchema !== "object" || propSchema === null)
    return null;
  const schema = propSchema;
  if (schema["type"] !== "array" && !("items" in schema))
    return null;
  const items = schema["items"];
  if (typeof items !== "object" || items === null)
    return null;
  const itemProps = items["properties"];
  if (typeof itemProps !== "object" || itemProps === null)
    return null;
  return itemProps;
}
function inspectRawProperties(properties, acc, options) {
  for (const [key, propSchema] of Object.entries(properties)) {
    const res = analyzeChildRefSchema(propSchema, options);
    if (res.isChildList) {
      acc.listRefs.add(key);
      continue;
    }
    if (res.isChild) {
      acc.singleRefs.add(key);
      continue;
    }
    if (options.childPropertyNames?.has(key) && typeof propSchema === "object" && propSchema !== null && propSchema["type"] === "string") {
      acc.singleRefs.add(key);
      continue;
    }
    const itemProps = rawArrayItemProperties(propSchema);
    if (!itemProps)
      continue;
    for (const [subKey, subSchema] of Object.entries(itemProps)) {
      const subRes = analyzeChildRefSchema(subSchema, options);
      if (subRes.isChild || subRes.isChildList || options.childPropertyNames?.has(subKey) && typeof subSchema === "object" && subSchema !== null && subSchema["type"] === "string") {
        acc.listRefs.add(key);
        addNestedRef(acc, key, subKey);
      }
    }
  }
}
function inspectComponentSchema(schema, acc, options) {
  if (!schema)
    return;
  const current = unwrapZodType(schema);
  if (current?._def?.typeName === "ZodObject" && typeof current._def.shape === "function") {
    const shape = current._def.shape();
    for (const [key, fieldSchema] of Object.entries(shape)) {
      inspectShapeField(key, fieldSchema, acc, options);
    }
    return;
  }
  if (typeof schema === "object" && schema !== null && "properties" in schema) {
    const rawProps = schema.properties;
    if (typeof rawProps === "object" && rawProps !== null) {
      inspectRawProperties(rawProps, acc, options);
    }
  }
}
function buildComponentRefMap(catalogOrComponents, options) {
  const refMap = {};
  const componentApis = typeof catalogOrComponents?.components?.values === "function" ? Array.from(catalogOrComponents.components.values()) : Array.isArray(catalogOrComponents) ? catalogOrComponents : Array.from(catalogOrComponents.values());
  for (const compApi of componentApis) {
    const acc = {
      singleRefs: /* @__PURE__ */ new Set(),
      listRefs: /* @__PURE__ */ new Set(),
      nestedRefs: {}
    };
    inspectComponentSchema(compApi.schema, acc, options);
    refMap[compApi.name] = {
      singleRefs: acc.singleRefs,
      listRefs: acc.listRefs,
      nestedRefs: acc.nestedRefs
    };
  }
  return refMap;
}

// node_modules/@a2ui/web_core/src/types/helpers.js
var MAX_FUNCTION_CALL_ARGS = 1e3;
var AnyComponentSchema = external_exports.object({
  "component": external_exports.string().describe("The type name of the component."),
  "id": external_exports.lazy(() => ComponentIdSchema).optional(),
  "weight": external_exports.number().optional()
}).passthrough().describe("A generic A2UI component definition.");

// node_modules/@a2ui/web_core/src/state/resolve-surface-catalog.js
function resolveSurfaceCatalog(surface, catalogId, subject) {
  if (catalogId !== void 0) {
    const found = surface.availableCatalogs?.get(catalogId);
    if (!found) {
      throw new A2uiCatalogError(`Catalog not found: ${catalogId}`);
    }
    return found;
  }
  if (!surface.defaultCatalog) {
    throw new A2uiCatalogError(`${subject} names no catalogId and surface '${surface.id}' has no default catalogId.`);
  }
  return surface.defaultCatalog;
}

// node_modules/@a2ui/web_core/src/resolution/data-context.js
var schemaKeysCache = /* @__PURE__ */ new WeakMap();
var SYSTEM_FUNCTION_CATALOG_ID = "a2ui:system-functions";
var systemFunctionCatalog;
function getSystemFunctionCatalog() {
  systemFunctionCatalog ??= new Catalog(SYSTEM_FUNCTION_CATALOG_ID, SpecVersion.V1_0, [], SYSTEM_FUNCTIONS);
  return systemFunctionCatalog;
}
function getKnownSchemaKeys(schema) {
  if (schemaKeysCache.has(schema)) {
    return schemaKeysCache.get(schema);
  }
  const result = (() => {
    let current = schema;
    while (current) {
      if (current instanceof external_exports.ZodObject || current._def?.typeName === "ZodObject") {
        if (current._def?.unknownKeys === "passthrough") {
          return null;
        }
        const shape = typeof current.shape === "function" ? current.shape() : current.shape;
        if (shape && typeof shape === "object") {
          return new Set(Object.keys(shape));
        }
        return null;
      }
      if (current instanceof external_exports.ZodEffects || current._def?.typeName === "ZodEffects") {
        current = current._def.schema;
        continue;
      }
      if (current instanceof external_exports.ZodOptional || current instanceof external_exports.ZodNullable || current._def?.typeName === "ZodOptional" || current._def?.typeName === "ZodNullable") {
        current = current._def.innerType;
        continue;
      }
      if (current instanceof external_exports.ZodDefault || current._def?.typeName === "ZodDefault") {
        current = current._def.innerType;
        continue;
      }
      if (current instanceof external_exports.ZodCatch || current._def?.typeName === "ZodCatch") {
        current = current._def.innerType;
        continue;
      }
      if (current instanceof external_exports.ZodIntersection || current._def?.typeName === "ZodIntersection") {
        const leftKeys = getKnownSchemaKeys(current._def.left);
        const rightKeys = getKnownSchemaKeys(current._def.right);
        if (!leftKeys || !rightKeys) {
          return null;
        }
        return /* @__PURE__ */ new Set([...leftKeys, ...rightKeys]);
      }
      if (current instanceof external_exports.ZodUnion || current._def?.typeName === "ZodUnion") {
        const options = current._def.options;
        const allKeys = /* @__PURE__ */ new Set();
        for (const opt of options) {
          const k2 = getKnownSchemaKeys(opt);
          if (!k2)
            return null;
          for (const key of k2) {
            allKeys.add(key);
          }
        }
        return allKeys;
      }
      return null;
    }
    return null;
  })();
  schemaKeysCache.set(schema, result);
  return result;
}
function validateFunctionArgs(functionName, rawArgs, catalog) {
  if (!rawArgs || typeof rawArgs !== "object" || Array.isArray(rawArgs)) {
    return;
  }
  const suppliedKeys = Object.keys(rawArgs);
  if (suppliedKeys.length > MAX_FUNCTION_CALL_ARGS) {
    throw new A2uiExpressionError(`Function call '${functionName}' exceeds maximum allowed arguments count (${MAX_FUNCTION_CALL_ARGS})`, functionName);
  }
  const fn = catalog?.functions?.get?.(functionName) ?? (functionName === "@index" && catalog?.protocolVersion && isAtLeastVersion(catalog.protocolVersion, SpecVersion.V1_0) ? IndexApi : void 0);
  if (!fn?.schema) {
    return;
  }
  const knownKeys = getKnownSchemaKeys(fn.schema);
  if (!knownKeys) {
    return;
  }
  for (const key of suppliedKeys) {
    if (!knownKeys.has(key)) {
      throw new A2uiExpressionError(`Unknown argument '${key}' passed to function '${functionName}'`, functionName);
    }
  }
  if (suppliedKeys.length > knownKeys.size) {
    throw new A2uiExpressionError(`Too many arguments for function '${functionName}': expected at most ${knownKeys.size}, received ${suppliedKeys.length}`, functionName);
  }
}
function isSingleAtKey(key) {
  return key.startsWith("@") && !key.startsWith("@@");
}
function unescapeObjectKey(key) {
  return key.startsWith("@@") ? key.slice(1) : key;
}
var RESERVED_DIRECTIVES = /* @__PURE__ */ new Set(["@path", "@call"]);
function validateReservedDirectives(keys, protocolVersion) {
  if (!isAtLeastVersion(protocolVersion, "1.0")) {
    return;
  }
  for (const key of keys) {
    if (isSingleAtKey(key) && !RESERVED_DIRECTIVES.has(key)) {
      throw new A2uiValidationError(`Unrecognized reserved protocol directive '${key}' in v1.0 dynamic object. Reserved keys must be in ${Array.from(RESERVED_DIRECTIVES).join(", ")}, or escaped with prefix doubling (e.g. '@${key}').`, void 0, "INVALID_RESERVED_KEY");
    }
  }
}
function resolveContextIndex(startCtx) {
  let ctx = startCtx;
  while (ctx) {
    if (ctx.explicitIndex !== void 0 && Number.isFinite(ctx.explicitIndex)) {
      return ctx.explicitIndex;
    }
    if (!(ctx instanceof DataContext) && typeof ctx.getIndex === "function") {
      const idx = ctx.getIndex();
      if (idx !== void 0 && Number.isFinite(idx)) {
        return idx;
      }
    }
    if (!(ctx instanceof DataContext) && typeof ctx.index === "number" && Number.isFinite(ctx.index)) {
      return ctx.index;
    }
    if (typeof ctx.path === "string") {
      const parts = ctx.path.split("/").filter(Boolean);
      if (parts.length > 0 && /^\d+$/.test(parts[parts.length - 1])) {
        return parseInt(parts[parts.length - 1], 10);
      }
    }
    ctx = ctx.parent;
  }
  return void 0;
}
var MAX_DYNAMIC_VALUE_DEPTH = 1e3;
var DataContext = class _DataContext {
  /**
   * Initializes a new DataContext instance.
   *
   * @param surface The surface model or data model this context belongs to.
   * @param path The absolute path in the DataModel that this context is scoped to.
   * @param index Optional explicit collection iteration index.
   * @param parent Optional parent DataContext in the scope chain.
   */
  constructor(surface, path, index, parent) {
    this.path = path;
    this._isUserActivated = false;
    this._isPassiveEvaluation = false;
    this._isActionExecution = false;
    if (surface instanceof DataModel) {
      this.surface = void 0;
      this.dataModel = surface;
      this.functionInvoker = () => void 0;
    } else {
      this.surface = surface;
      this.dataModel = surface.dataModel;
      this.functionInvoker = surface.defaultCatalog?.invoker ?? ((name, ...rest) => resolveSurfaceCatalog(surface, void 0, `Function call '${name}'`).invoker(name, ...rest));
    }
    this.explicitIndex = index;
    this.parent = parent;
    this.warnedPaths = parent ? parent.warnedPaths : /* @__PURE__ */ new Set();
  }
  /** Whether the current function evaluation was initiated by an active user action. */
  get isUserActivated() {
    return this._isUserActivated || Boolean(this.parent?.isUserActivated);
  }
  /** Whether the current function evaluation is running inside a passive reactive binding. */
  get isPassiveEvaluation() {
    return this._isPassiveEvaluation || Boolean(this.parent?.isPassiveEvaluation);
  }
  /**
   * Returns the 0-based iteration index if this context (or an ancestor context)
   * is scoped to a collection template item, or `undefined` when outside any
   * collection template scope.
   *
   * Mirrors `DataContext.index` in Python: checks an explicit `explicitIndex` first,
   * then checks strictly the trailing segment of `ctx.path`, walking `ctx.parent`.
   *
   * @returns The 0-based iteration index, or `undefined` if outside an iteration scope.
   */
  getIndex() {
    return resolveContextIndex(this);
  }
  /** Active iteration index if inside a collection template scope, or `undefined`. */
  get index() {
    return this.getIndex();
  }
  /**
   * Mutates the underlying DataModel at the specified path.
   *
   * @param path JSON pointer path, resolved relative to this context's `path` if not absolute.
   * @param value New value to store in the DataModel.
   */
  set(path, value) {
    const absolutePath = this.resolvePath(path);
    const preserveNull = Boolean(this.surfaceProtocolVersion && !this.atLeastV10);
    this.dataModel.set(absolutePath, value, preserveNull);
  }
  /**
   * Protocol version of the bound surface.
   *
   * Reads the surface's own version, which is set even when the surface has
   * no default catalog, and falls back to the default catalog's version for
   * surface-like objects that do not carry one.
   */
  get surfaceProtocolVersion() {
    return this.surface?.protocolVersion ?? this.surface?.defaultCatalog?.protocolVersion;
  }
  /** Whether this context targets A2UI protocol v1.0 or newer. */
  get atLeastV10() {
    if (this._cachedAtLeastV10 === void 0) {
      this._cachedAtLeastV10 = isAtLeastVersion(this.surfaceProtocolVersion, "1.0");
    }
    return this._cachedAtLeastV10;
  }
  /**
   * Checks whether an object represents a data binding.
   *
   * In v1.0, data bindings must use `@path`. In v0.9, data bindings use `path`.
   *
   * @param val Candidate object to inspect.
   * @returns Whether the object represents a valid data binding for this context's protocol version.
   */
  isDataBindingObject(val) {
    const hasPath = this.atLeastV10 ? "@path" in val && typeof val["@path"] === "string" : "path" in val && typeof val.path === "string";
    return hasPath && !("componentId" in val);
  }
  /**
   * Checks whether an object represents a function call.
   *
   * In v1.0, function calls must use `@call`. In v0.9, function calls use `call`.
   *
   * @param val Candidate object to inspect.
   * @returns Whether the object represents a valid function call for this context's protocol version.
   */
  isFunctionCallObject(val) {
    return this.atLeastV10 ? "@call" in val && typeof val["@call"] === "string" : "call" in val && typeof val.call === "string";
  }
  /**
   * Checks whether a value contains any dynamic parts (path bindings,
   * function calls, or escaped/reserved directive keys) at any nesting depth.
   *
   * @param value The value or data structure to inspect.
   * @returns Whether the value contains dynamic elements requiring resolution.
   */
  containsDynamicValue(value) {
    if (value === null || typeof value !== "object" || isThenable(value)) {
      return false;
    }
    if (Array.isArray(value)) {
      return value.some((item) => this.containsDynamicValue(item));
    }
    const rec = value;
    if (this.isDataBindingObject(rec) || this.isFunctionCallObject(rec)) {
      return true;
    }
    if (this.atLeastV10) {
      for (const k2 of Object.keys(rec)) {
        if (k2.startsWith("@")) {
          return true;
        }
      }
    }
    return Object.values(rec).some((v3) => this.containsDynamicValue(v3));
  }
  /**
   * Emits a warning if a data binding path does not exist in the data model.
   *
   * @param absolutePath Absolute JSON pointer path to check.
   */
  emitMissingDataBindingWarning(absolutePath) {
    if (typeof this.dataModel?.hasPath === "function" && !this.dataModel.hasPath(absolutePath) && !this.warnedPaths.has(absolutePath)) {
      this.warnedPaths.add(absolutePath);
      void this.surface?.dispatchWarning?.({
        code: "MISSING_DATA_BINDING",
        path: absolutePath,
        message: `Preflight DataBinding Warning: The bound JSON Pointer '${absolutePath}' does not physically exist in the active DataModel. Evaluating to None.`
      });
    }
  }
  /**
   * Synchronously evaluates a DynamicValue into its concrete runtime value.
   *
   * Evaluates the value once at the current moment without creating reactive subscriptions.
   * Use `subscribeDynamicValue` for reactive updates.
   *
   * A pending Promise, such as the result of an asynchronous function nested
   * in the arguments of another call, is returned unchanged: the invoker of
   * the enclosing call awaits it before validating the arguments.
   * @param value The DynamicValue object or raw value from the A2UI JSON payload.
   * @param depth The current recursion depth when evaluating nested arguments or expressions.
   * @param userActivated Whether the evaluation was initiated by an active user action.
   * @returns The synchronously resolved value.
   */
  resolveDynamicValue(value, depth = 0, userActivated = false) {
    if (depth > MAX_DYNAMIC_VALUE_DEPTH) {
      const err = new A2uiExpressionError(`Maximum dynamic value nesting depth exceeded (${MAX_DYNAMIC_VALUE_DEPTH})`);
      this.dispatchExpressionError(err, "DynamicValue");
      return void 0;
    }
    if (value === null || typeof value !== "object" || isThenable(value)) {
      return value;
    }
    if (Array.isArray(value)) {
      if (!this.containsDynamicValue(value)) {
        return value;
      }
      return value.map((item) => this.resolveDynamicValue(item, depth + 1, userActivated));
    }
    const rec = value;
    if (this.isDataBindingObject(rec)) {
      const bindingPath = rec["@path"] ?? rec.path;
      const absolutePath = this.resolvePath(bindingPath);
      const val = this.dataModel.get(absolutePath);
      if (val === void 0) {
        this.emitMissingDataBindingWarning(absolutePath);
      }
      return val;
    }
    if (this.isFunctionCallObject(rec)) {
      return this.resolveFunctionCallValue(rec, depth, userActivated);
    }
    return this.resolvePlainObjectValue(rec, depth, userActivated);
  }
  /**
   * Resolves a function call by validating arguments and invoking the function.
   *
   * An asynchronous function returns a pending Promise. At the top level of an
   * expression the Promise is returned with its rejection turned into an
   * `EXPRESSION_ERROR` on the surface, since nothing else would report it;
   * nested in the arguments of another call it is returned as it is, so the
   * enclosing call awaits it and a rejection reaches that call's handler.
   *
   * @param call Function call definition to execute.
   * @param depth Current recursion depth for nested expression tracking.
   * @param userActivated Whether the evaluation was initiated by an active user action.
   * @returns The resolved function return value.
   */
  resolveFunctionCallValue(call, depth = 0, userActivated = false) {
    const callName = call["@call"] ?? call.call;
    const targetCatalog = this.resolveCallCatalog(callName, call);
    if (!targetCatalog) {
      return void 0;
    }
    try {
      validateFunctionArgs(callName, call.args, targetCatalog);
    } catch (e7) {
      this.dispatchExpressionError(e7, callName);
      return void 0;
    }
    const args = {};
    for (const [key, argVal] of Object.entries(call.args ?? {})) {
      args[key] = this.resolveDynamicValue(argVal, depth + 1, userActivated);
    }
    const abortController = new AbortController();
    const prevActivated = this._isUserActivated;
    this._isUserActivated = prevActivated || userActivated;
    let result;
    try {
      result = this.evaluateFunctionReactive(callName, args, targetCatalog.invoker, abortController.signal);
    } finally {
      this._isUserActivated = prevActivated;
    }
    if (result === void 0) {
      return void 0;
    }
    if (isThenable(result)) {
      if (depth > 0) {
        return result;
      }
      return Promise.resolve(result).then((value) => value, (e7) => {
        this.dispatchExpressionError(e7, callName);
        return void 0;
      });
    }
    return isSignal(result) ? peekValue(result) : result;
  }
  /**
   * Recursively resolves dynamic values nested inside a plain object.
   *
   * @param rec Plain object dictionary to resolve.
   * @param depth Current recursion depth for nested expression tracking.
   * @param userActivated Whether the evaluation was initiated by an active user action.
   * @returns A copy of the object with all nested dynamic values resolved.
   */
  resolvePlainObjectValue(rec, depth = 0, userActivated = false) {
    if (this.atLeastV10) {
      validateReservedDirectives(Object.keys(rec), this.surfaceProtocolVersion);
    }
    if (!this.containsDynamicValue(rec)) {
      return rec;
    }
    const resolved = {};
    for (const [k2, v3] of Object.entries(rec)) {
      const key = this.atLeastV10 ? unescapeObjectKey(k2) : k2;
      resolved[key] = this.resolveDynamicValue(v3, depth + 1, userActivated);
    }
    return resolved;
  }
  /**
   * Reactively listens to changes in a DynamicValue.
   *
   * Whenever the underlying data or function dependencies change, the `onChange`
   * callback fires with the freshly evaluated result.
   *
   * @template V Expected type of the resolved value.
   * @param value The DynamicValue or raw value to evaluate and observe.
   * @param onChange Callback fired whenever the evaluated result changes.
   * @returns A subscription containing the current value and an `unsubscribe` method.
   */
  subscribeDynamicValue(value, onChange) {
    const sig = this.resolveSignal(value);
    let isSync = true;
    let currentValue = peekValue(sig);
    const dispose = effect(() => {
      const val = getValue(sig);
      currentValue = val;
      if (!isSync) {
        onChange(val);
      }
    });
    isSync = false;
    return {
      get value() {
        return currentValue;
      },
      unsubscribe: () => {
        dispose();
        sig.unsubscribe?.();
      }
    };
  }
  /**
   * Resolves a DynamicValue into a reactive Signal.
   *
   * Recursively resolves any nested path bindings or function calls into a
   * single reactive `Signal`. Changes to underlying data or function dependencies
   * cause the signal's value to update.
   *
   * @template V Expected type of the signal value.
   * @param value The DynamicValue or raw value to evaluate and observe.
   * @param depth The current recursion depth when evaluating nested arguments or expressions.
   * @returns A reactive Signal containing the result of the evaluation.
   */
  resolveSignal(value, depth = 0) {
    if (depth > MAX_DYNAMIC_VALUE_DEPTH) {
      const err = new A2uiExpressionError(`Maximum dynamic value nesting depth exceeded (${MAX_DYNAMIC_VALUE_DEPTH})`);
      this.dispatchExpressionError(err, typeof value === "object" && value && ("@call" in value || "call" in value) ? value["@call"] ?? value.call : "DynamicValue");
      return signal(void 0);
    }
    if (typeof value !== "object" || value === null) {
      return signal(value);
    }
    if (isThenable(value)) {
      const pendingSig = signal(void 0);
      this.settleIntoSignal(pendingSig, value, "DynamicValue");
      return pendingSig;
    }
    if (Array.isArray(value)) {
      if (!this.containsDynamicValue(value)) {
        return signal(value);
      }
      const itemSignals = value.map((item) => this.resolveSignal(item, depth + 1));
      const resultSig = computed(() => itemSignals.map((s5) => getValue(s5)));
      resultSig.unsubscribe = () => {
        for (const s5 of itemSignals) {
          s5.unsubscribe?.();
        }
      };
      return resultSig;
    }
    const rec = value;
    if (this.isDataBindingObject(rec)) {
      const bindingPath = rec["@path"] ?? rec.path;
      const absolutePath = this.resolvePath(bindingPath);
      this.emitMissingDataBindingWarning(absolutePath);
      return this.dataModel.getSignal(absolutePath);
    }
    if (this.isFunctionCallObject(rec)) {
      const call = rec;
      const callName = call["@call"] ?? call.call;
      const targetCatalog = this.resolveCallCatalog(callName, call);
      if (!targetCatalog) {
        return signal(void 0);
      }
      try {
        validateFunctionArgs(callName, call.args, targetCatalog);
      } catch (e7) {
        this.dispatchExpressionError(e7, callName);
        return signal(void 0);
      }
      const argSignals = {};
      for (const [key, argVal] of Object.entries(call.args ?? {})) {
        argSignals[key] = this.resolveSignal(argVal, depth + 1);
      }
      if (Object.keys(argSignals).length === 0) {
        const abortController2 = new AbortController();
        const result = this.evaluateFunctionPassive({
          name: callName,
          args: {},
          abortSignal: abortController2.signal,
          invoker: targetCatalog.invoker
        });
        let sig;
        if (isSignal(result)) {
          sig = result;
        } else if (isThenable(result)) {
          const pendingSig = signal(void 0);
          this.settleIntoSignal(pendingSig, result, callName, abortController2.signal);
          sig = pendingSig;
        } else {
          sig = signal(result);
        }
        sig.unsubscribe = () => abortController2.abort();
        return sig;
      }
      const keys = Object.keys(argSignals);
      const resultSig = signal(void 0);
      let abortController;
      let innerUnsubscribe;
      const argsSig = computed(() => {
        const argsRecord = {};
        for (let i7 = 0; i7 < keys.length; i7++) {
          argsRecord[keys[i7]] = getValue(argSignals[keys[i7]]);
        }
        return argsRecord;
      });
      const stopper = effect(() => {
        try {
          const args = getValue(argsSig);
          if (abortController)
            abortController.abort();
          if (innerUnsubscribe) {
            innerUnsubscribe();
            innerUnsubscribe = void 0;
          }
          abortController = new AbortController();
          const res = this.evaluateFunctionPassive({
            name: callName,
            args,
            abortSignal: abortController.signal,
            invoker: targetCatalog.invoker
          });
          if (isSignal(res)) {
            setValue(resultSig, peekValue(res));
            innerUnsubscribe = effect(() => {
              setValue(resultSig, getValue(res));
            });
          } else if (isThenable(res)) {
            this.settleIntoSignal(resultSig, res, callName, abortController.signal);
          } else {
            setValue(resultSig, res);
          }
        } catch (e7) {
          this.dispatchExpressionError(e7, callName);
          setValue(resultSig, void 0);
        }
      });
      resultSig.unsubscribe = () => {
        stopper();
        if (innerUnsubscribe)
          innerUnsubscribe();
        if (abortController)
          abortController.abort();
        for (let i7 = 0; i7 < keys.length; i7++) {
          argSignals[keys[i7]].unsubscribe?.();
        }
      };
      return resultSig;
    }
    if (this.atLeastV10) {
      validateReservedDirectives(Object.keys(rec), this.surfaceProtocolVersion);
    }
    if (!this.containsDynamicValue(rec)) {
      return signal(value);
    }
    const entrySignals = Object.entries(rec).map(([k2, v3]) => {
      const key = this.atLeastV10 ? unescapeObjectKey(k2) : k2;
      return [key, this.resolveSignal(v3, depth + 1)];
    });
    const objSig = computed(() => {
      const resolved = {};
      for (const [k2, s5] of entrySignals) {
        resolved[k2] = getValue(s5);
      }
      return resolved;
    });
    const prevUnsubscribe = objSig.unsubscribe?.bind(objSig);
    objSig.unsubscribe = () => {
      prevUnsubscribe?.();
      for (const [, s5] of entrySignals) {
        s5.unsubscribe?.();
      }
    };
    return objSig;
  }
  handleInvalidAction(message) {
    this.surface?.dispatchError({
      code: "INVALID_ACTION",
      surfaceId: this.surface?.id,
      message
    });
    return void 0;
  }
  resolveActionEventObject(ev) {
    if (typeof ev.name !== "string" || ev.name.length === 0) {
      return this.handleInvalidAction("Action event is missing required 'name' string.");
    }
    let resolvedContext = void 0;
    if (ev.context && typeof ev.context === "object" && !Array.isArray(ev.context)) {
      resolvedContext = {};
      for (const [key, value] of Object.entries(ev.context)) {
        resolvedContext[key] = this.resolveDynamicValue(value);
      }
    }
    const resolvedEvent = {
      ...ev,
      ...resolvedContext !== void 0 ? { context: resolvedContext } : {}
    };
    if (ev.context === void 0) {
      delete resolvedEvent.context;
    }
    if (ev.userMessage !== void 0) {
      const resolvedMsg = this.resolveDynamicValue(ev.userMessage);
      if (typeof resolvedMsg !== "string") {
        return this.handleInvalidAction("Action event 'userMessage' must resolve to a string.");
      }
      resolvedEvent.userMessage = resolvedMsg;
    }
    return resolvedEvent;
  }
  executeActionFunctionCall(action, options) {
    let callTarget = "functionCall" in action ? action.functionCall : action;
    if (callTarget && typeof callTarget === "object") {
      const callName = callTarget["@call"] ?? callTarget.call;
      if (typeof callName === "string") {
        callTarget = this.atLeastV10 && !("@call" in callTarget) ? { "@call": callName, ...callTarget } : !this.atLeastV10 && !("call" in callTarget) ? { call: callName, ...callTarget } : callTarget;
      }
    }
    const userActivated = options?.userActivation ?? true;
    const prevAction = this._isActionExecution;
    const prevPassive = this._isPassiveEvaluation;
    this._isActionExecution = true;
    if (options?.userActivation === false) {
      this._isPassiveEvaluation = true;
    }
    try {
      return this.resolveDynamicValue(callTarget, 0, userActivated);
    } finally {
      this._isActionExecution = prevAction;
      this._isPassiveEvaluation = prevPassive;
    }
  }
  /**
   * Resolves an action by evaluating its top-level dynamic values.
   *
   * For event actions, resolves each value in the context map.
   * For function call actions, evaluates the function call.
   *
   * @param action The Action object to resolve.
   * @returns The resolved action payload or function execution result.
   */
  resolveAction(action, options) {
    if ("event" in action && typeof action.event === "object" && action.event !== null) {
      const resolvedEvent = this.resolveActionEventObject(action.event);
      if (resolvedEvent === void 0) {
        return void 0;
      }
      return {
        ...action,
        event: resolvedEvent
      };
    }
    if ("name" in action) {
      return this.resolveActionEventObject(action);
    }
    if ("functionCall" in action || "call" in action || "@call" in action) {
      return this.executeActionFunctionCall(action, options);
    }
    return action;
  }
  /**
   * Resolves the catalog against which a function call executes.
   *
   * From v1.0, reserved `@`-prefixed system functions such as `@index` belong
   * to no catalog, so they resolve to the built-in system function set
   * whatever the surface's catalogs are. Before v1.0 a call cannot select a
   * catalog, so a `catalogId` on it is ignored and the call runs in the
   * surface catalog.
   *
   * @param name Name of the function being called.
   * @param catalogId Identifier of the catalog named by the call, if specified.
   * @param namesCatalog Whether the call carries a `catalogId` key, whatever
   *   its value.
   * @returns The resolved Catalog instance, or the surface default catalog if omitted.
   * @throws {A2uiExpressionError} If, from v1.0, a call to a reserved `@`
   *   system function names a catalog: such a function belongs to no catalog,
   *   so the call is a malformed expression.
   * @throws {A2uiCatalogError} If the call names a catalog ID that cannot be
   *   resolved on this surface, or names none on a surface without a default
   *   catalog. This is reported as a catalog fault rather than a missing
   *   function, since the function may exist in a catalog that is not
   *   available here.
   */
  resolveFunctionCatalog(name, catalogId, namesCatalog) {
    if (this.atLeastV10 && name.startsWith("@")) {
      if (namesCatalog) {
        throw new A2uiExpressionError(`System function '${name}' belongs to no catalog and must not name a catalogId.`, name);
      }
      return getSystemFunctionCatalog();
    }
    if (!this.surface) {
      throw new A2uiCatalogError(`No surface available to resolve catalog: ${catalogId ?? "default"}`);
    }
    return resolveSurfaceCatalog(this.surface, this.atLeastV10 ? catalogId : void 0, `Function call '${name}'`);
  }
  /**
   * Resolves the catalog a function call runs in, dispatching a
   * `CATALOG_ERROR` to the surface when it cannot be resolved, and an
   * `EXPRESSION_ERROR` for any other failure, such as a system function call
   * that names a catalog.
   *
   * @param name Name of the function being called.
   * @param call The function call, whose `catalogId` selects the catalog.
   * @returns The resolved Catalog instance, or `undefined` if resolution failed.
   */
  resolveCallCatalog(name, call) {
    try {
      return this.resolveFunctionCatalog(name, call.catalogId, "catalogId" in call);
    } catch (e7) {
      if (e7 instanceof A2uiCatalogError && this.surface) {
        this.surface.dispatchError({
          code: "CATALOG_ERROR",
          message: e7.message,
          expression: name
        });
      } else {
        this.dispatchExpressionError(e7, name);
      }
      return void 0;
    }
  }
  evaluateFunctionPassive(options) {
    const { name, args, invoker, abortSignal } = options;
    const prevPassive = this._isPassiveEvaluation;
    this._isPassiveEvaluation = true;
    try {
      return this.evaluateFunctionReactive(name, args, invoker, abortSignal);
    } finally {
      this._isPassiveEvaluation = prevPassive;
    }
  }
  /**
   * Evaluates a catalog function and returns its reactive Signal or static value.
   *
   * Invokes the function with this context through the invoker of the
   * catalog the call resolved to, and dispatches an `EXPRESSION_ERROR` if
   * execution throws.
   *
   * @template V Expected return type of the function evaluation.
   * @param name Name of the function to evaluate.
   * @param args Resolved arguments to pass to the function.
   * @param invoker Invoker of the catalog the call resolved to.
   * @param abortSignal Optional abort signal to cancel asynchronous execution.
   * @returns The evaluated result as a reactive `Signal` or static value, or `undefined` on failure.
   */
  evaluateFunctionReactive(name, args, invoker, abortSignal) {
    try {
      return invoker(name, args, this, abortSignal);
    } catch (e7) {
      this.dispatchExpressionError(e7, name);
      return void 0;
    }
  }
  /**
   * Writes the settled value of a pending function result into a signal.
   *
   * The signal keeps its current value while the result is pending. A
   * completion that arrives after `abortSignal` fired is ignored, since the
   * arguments changed or the subscription ended in the meantime. A rejection
   * is dispatched as an `EXPRESSION_ERROR` and resets the signal, as a
   * synchronous failure of the same call would.
   *
   * @param sig The signal that exposes the result.
   * @param pending The pending result.
   * @param name Name of the function being evaluated.
   * @param abortSignal Signal that marks the evaluation as stale.
   */
  settleIntoSignal(sig, pending, name, abortSignal) {
    Promise.resolve(pending).then((value) => {
      if (!abortSignal?.aborted) {
        setValue(sig, value);
      }
    }, (e7) => {
      if (!abortSignal?.aborted) {
        this.dispatchExpressionError(e7, name);
        setValue(sig, void 0);
      }
    });
  }
  /**
   * Dispatches an evaluation failure to the surface as an `EXPRESSION_ERROR`.
   *
   * Every failure other than an unresolvable catalog uses this code, including
   * a function missing from the catalog the call resolved to and an unknown
   * system function. Unresolvable catalogs are reported by
   * {@link resolveCallCatalog} as `CATALOG_ERROR`.
   *
   * @param e The error thrown while evaluating the call.
   * @param name Name of the function being evaluated.
   */
  dispatchExpressionError(e7, name) {
    if (!this.surface)
      return;
    if (e7 instanceof external_exports.ZodError || typeof e7 === "object" && e7 !== null && e7.name === "ZodError") {
      const zodErr = e7;
      const err = new A2uiExpressionError(`Validation failed for function '${name}': ${zodErr.message}`, name, zodErr.errors ?? zodErr.issues);
      this.surface.dispatchError({
        code: "EXPRESSION_ERROR",
        message: err.message,
        expression: name,
        details: err.details
      });
    } else if (e7 instanceof A2uiExpressionError) {
      this.surface.dispatchError({
        code: "EXPRESSION_ERROR",
        message: e7.message,
        expression: e7.expression,
        details: e7.details
      });
    } else {
      const errObj = typeof e7 === "object" && e7 !== null ? e7 : {};
      this.surface.dispatchError({
        code: this._isActionExecution ? "EXECUTION_ERROR" : "EXPRESSION_ERROR",
        message: errObj.message ?? `An unexpected error occurred in function ${name}.`,
        expression: name
      });
    }
  }
  /**
   * Creates a child DataContext scoped to a deeper relative path.
   *
   * @param relativePath The path relative to the current context's path.
   * @param index Optional explicit iteration index for this nested scope.
   * @returns A new DataContext instance pointing to the resolved absolute path.
   */
  nested(relativePath, index) {
    const newPath = this.resolvePath(relativePath);
    return new _DataContext(this.surface ?? this.dataModel, newPath, index, this);
  }
  resolvePath(path) {
    if (path.startsWith("/")) {
      return path;
    }
    let base = this.path;
    if (base.endsWith("/") && base.length > 1) {
      base = base.slice(0, -1);
    }
    if (path === "" || path === ".") {
      return base || "/";
    }
    if (base === "/")
      base = "";
    return `${base}/${path}`;
  }
};

// node_modules/@a2ui/web_core/src/v1_0/schema/common-types.js
var ComponentIdSchema2 = markChildRef(external_exports.string().describe("REF:#/$defs/ComponentId|The unique identifier for a component, used for both definitions and references within the same surface."), "component-id");
var CallIdSchema2 = external_exports.string().describe("REF:#/$defs/CallId|The unique identifier for a function call.");
var DataBindingSchema2 = external_exports.object({
  "@path": external_exports.string().describe("A JSON Pointer path to a value in the data model.").optional(),
  "path": external_exports.string().describe("A JSON Pointer path to a value in the data model.").optional()
}).refine((data) => data["@path"] !== void 0 || data.path !== void 0, {
  message: "Either '@path' or 'path' must be provided."
}).describe("REF:#/$defs/DataBinding");
var FunctionCommonSchema2 = external_exports.object({
  "@call": external_exports.string().describe("The name of the function to call.").optional(),
  "call": external_exports.string().describe("The name of the function to call.").optional(),
  "catalogId": external_exports.string().describe("The catalog ID for this function, overriding any surface-level default catalogId.").optional()
}).refine((data) => data["@call"] !== void 0 || data.call !== void 0, {
  message: "Either '@call' or 'call' must be provided."
}).describe("REF:#/$defs/FunctionCommon|Baseline envelope properties common to all function calls. Function-specific argument schemas ('args') are defined individually by each function in the active catalog.");
var FunctionCallSchema2 = external_exports.record(external_exports.string(), external_exports.any()).and(external_exports.intersection(FunctionCommonSchema2, external_exports.any())).describe("REF:#/$defs/FunctionCall|Invokes a named function, combining common function properties with the catalog function definition.");
var DynamicStringSchema2 = external_exports.union([external_exports.string(), DataBindingSchema2, FunctionCallSchema2]).describe("REF:#/$defs/DynamicString|Represents a string");
var DynamicBooleanSchema2 = external_exports.union([external_exports.boolean(), DataBindingSchema2, FunctionCallSchema2]).describe("REF:#/$defs/DynamicBoolean|A boolean value that can be a literal, a path, or a function call returning a boolean or a validation result object (coerced to its 'valid' boolean).");
var AccessibilityAttributesSchema2 = external_exports.object({
  "label": DynamicStringSchema2.optional(),
  "description": DynamicStringSchema2.optional(),
  "live": external_exports.enum(["off", "polite", "assertive"]).describe("Controls screen reader announcements for dynamic updates (WAI-ARIA aria-live). 'polite' waits for user pause; 'assertive' interrupts immediately for alerts.").default("off"),
  "hidden": DynamicBooleanSchema2.optional()
}).strict().describe("REF:#/$defs/AccessibilityAttributes|Attributes to enhance accessibility when using assistive technologies like screen readers or model understanding.");
var ExtensionsSchema2 = external_exports.record(external_exports.string(), external_exports.unknown()).superRefine((value, ctx) => {
  for (const key in value) {
    if (!isValidUax31Identifier(key)) {
      ctx.addIssue({
        path: [key],
        code: external_exports.ZodIssueCode.custom,
        message: `Invalid extension key "${key}": Keys MUST be Unicode identifiers (UAX #31).`
      });
    }
  }
}).describe("REF:#/$defs/Extensions|Optional extension metadata. Keys MUST be Unicode identifiers (UAX #31). Keys starting with 'a2ui_' are reserved for official extensions.");
var ComponentCommonSchema2 = external_exports.object({
  "id": ComponentIdSchema2,
  "catalogId": external_exports.string().describe("The catalog ID for this component, overriding any surface-level default catalogId.").optional(),
  "accessibility": AccessibilityAttributesSchema2.optional(),
  "metadata": external_exports.object({ "extensions": ExtensionsSchema2.optional() }).strict().describe("Optional component-level metadata for vendor extensions.").optional()
}).describe("REF:#/$defs/ComponentCommon");
var ChildListSchema2 = markChildRef(external_exports.union([
  external_exports.array(ComponentIdSchema2).describe("A static list of child component IDs."),
  external_exports.object({
    "componentId": ComponentIdSchema2,
    "path": external_exports.string().describe("The path to the list of component property objects in the data model.")
  }).strict().describe("A template for generating a dynamic list of children from a data model list. The `componentId` is the component to use as a template.")
]).describe("REF:#/$defs/ChildList"), "child-list");
var DynamicValueSchema2 = external_exports.union([
  external_exports.string(),
  external_exports.number(),
  external_exports.boolean(),
  external_exports.array(external_exports.any()),
  external_exports.record(external_exports.string(), external_exports.unknown()).refine((obj) => !obj || !("@path" in obj) && !("path" in obj) && !("@call" in obj) && !("call" in obj)),
  DataBindingSchema2,
  FunctionCallSchema2
]).describe("REF:#/$defs/DynamicValue|A value that can be a literal, a path, or a function call returning any type.");
var DynamicNumberSchema2 = external_exports.union([external_exports.number(), DataBindingSchema2, FunctionCallSchema2]).describe("REF:#/$defs/DynamicNumber|Represents a value that can be either a literal number, a path to a number in the data model, or a function call returning a number.");
var DynamicStringListSchema2 = external_exports.union([external_exports.array(external_exports.string()), DataBindingSchema2, FunctionCallSchema2]).describe("REF:#/$defs/DynamicStringList|Represents a value that can be either a literal array of strings, a path to a string array in the data model, or a function call returning a string array.");
var IndexSystemFunctionSchema2 = external_exports.object({
  "@call": external_exports.literal("@index").optional(),
  "call": external_exports.literal("@index").optional(),
  "args": external_exports.object({ "offset": DynamicNumberSchema2.optional() }).optional()
}).refine((data) => data["@call"] !== void 0 || data.call !== void 0, {
  message: "Either '@call' or 'call' must be '@index'."
}).describe("REF:#/$defs/IndexSystemFunction");
var CheckRuleSchema2 = external_exports.object({
  "condition": external_exports.union([DataBindingSchema2, FunctionCallSchema2]).describe("Path or function call evaluating to a structured validation result object or a boolean."),
  "message": external_exports.string().describe("Optional fallback error message.").optional()
}).strict().describe("REF:#/$defs/CheckRule|A single validation check rule applied to an input component. The condition function or path evaluates to a structured validation result object or a boolean (which is coerced to a validation result object).");
var CheckableSchema2 = external_exports.object({
  "checks": external_exports.array(CheckRuleSchema2).describe("A list of validation check rules to perform on the component.").optional()
}).describe("REF:#/$defs/Checkable|Properties for components that support renderer-side checks.");
var ActionSchema2 = external_exports.union([
  external_exports.object({
    "event": external_exports.object({
      "name": external_exports.string().describe("The name of the action to be dispatched to the agent."),
      "userMessage": DynamicStringSchema2.optional(),
      "context": external_exports.record(external_exports.string(), DynamicValueSchema2).describe("A JSON object containing the key-value pairs for the action context. Values can be literals or paths. Use literal values unless the value must be dynamically bound to the data model. Do NOT use paths for static IDs.").optional()
    }).strict().describe("The event to dispatch to the agent.")
  }).strict().describe("Triggers an agent-side event."),
  external_exports.object({ "functionCall": FunctionCallSchema2 }).strict().describe("Executes a renderer or agent-side function.")
]).describe("REF:#/$defs/Action|Defines an interaction handler that can either trigger an agent-side event or execute a local renderer-side function.");
var SurfaceSchema2 = external_exports.object({ "component": external_exports.literal("Surface").optional(), "child": external_exports.literal("root").optional() }).strict().describe("REF:#/$defs/Surface|The reserved canonical container component representing an A2UI surface. The Surface component is immutable and always has 'child': 'root'.");
var FunctionResponseSchema2 = external_exports.object({
  "functionCallId": CallIdSchema2,
  "value": external_exports.any().describe("The return value of the function.").optional(),
  "error": external_exports.object({ "code": external_exports.string(), "message": external_exports.string() }).strict().describe("An error object indicating failure of the function execution.").optional()
}).strict().superRefine((val, ctx) => {
  const hasValue = "value" in val;
  const hasError = "error" in val;
  if (hasValue && hasError || !hasValue && !hasError) {
    ctx.addIssue({
      code: external_exports.ZodIssueCode.custom,
      message: 'FunctionResponse must have either "value" or "error", but not both or neither.'
    });
  }
}).describe("REF:#/$defs/FunctionResponse|The return response matching a callAgentFunction or callRendererFunction invocation.");

// node_modules/@a2ui/web_core/src/v1_0/functions/system_functions.js
var IndexApi = {
  /** Name of the system function. */
  name: "@index",
  /** Declared return type for catalog type checking. */
  returnType: "number",
  /** Scope of callers permitted to invoke this function. */
  allowedCallers: "rendererOnly",
  /**
   * Zod schema validating function arguments.
   *
   * `offset` is a DynamicNumber, so a payload may bind it to a path or a
   * nested call. Once resolved, the value is coerced to a number.
   */
  schema: external_exports.object({
    "offset": external_exports.union([DataBindingSchema2, FunctionCallSchema2, external_exports.coerce.number()]).optional()
  })
};
var IndexImplementation = createFunctionImplementation(IndexApi, (args, context) => {
  const offset = typeof args.offset === "number" && Number.isFinite(args.offset) ? args.offset : 0;
  const index = resolveContextIndex(context);
  if (index === void 0 || !Number.isFinite(index)) {
    throw new A2uiValidationError("@index function can only be evaluated inside a collection template iteration scope.");
  }
  return index + offset;
});
var SYSTEM_FUNCTIONS = [IndexImplementation];

// node_modules/@a2ui/web_core/src/catalog/validation_result_coercion.js
var DYNAMIC_BOOLEAN_REF = /^REF:[^|#]*#\/\$defs\/DynamicBoolean(?:\||$)/;
function validityOf(value) {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return void 0;
  }
  const valid = value.valid;
  return typeof valid === "boolean" ? valid : void 0;
}
function innerSchema2(schema) {
  const def = schema._def;
  switch (def.typeName) {
    case "ZodOptional":
    case "ZodNullable":
    case "ZodDefault":
    case "ZodCatch":
    case "ZodBranded":
    case "ZodReadonly":
      return def.innerType;
    case "ZodEffects":
      return def.schema;
    case "ZodLazy":
      return def.getter?.();
    default:
      return void 0;
  }
}
function isDynamicBooleanSchema(schema) {
  let current = schema;
  while (current) {
    if (typeof current.description === "string" && DYNAMIC_BOOLEAN_REF.test(current.description)) {
      return true;
    }
    current = innerSchema2(current);
  }
  return false;
}
function unwrap(schema) {
  let current = schema;
  for (let inner = innerSchema2(current); inner; inner = innerSchema2(current)) {
    current = inner;
  }
  return current;
}
function isPlainObject4(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
function coerceValidationResults(schema, value) {
  if (typeof value !== "object" || value === null) {
    return value;
  }
  if (isDynamicBooleanSchema(schema)) {
    return validityOf(value) ?? value;
  }
  const core = unwrap(schema);
  const def = core._def;
  if (def.typeName === "ZodArray" && Array.isArray(value) && def.type) {
    const itemSchema = def.type;
    let changed = false;
    const items = value.map((item) => {
      const coerced = coerceValidationResults(itemSchema, item);
      changed ||= coerced !== item;
      return coerced;
    });
    return changed ? items : value;
  }
  if (def.typeName === "ZodObject" && isPlainObject4(value)) {
    const shape = core.shape;
    let changed = false;
    const result = {};
    for (const [key, item] of Object.entries(value)) {
      const propertySchema2 = shape[key];
      const coerced = propertySchema2 ? coerceValidationResults(propertySchema2, item) : item;
      changed ||= coerced !== item;
      result[key] = coerced;
    }
    return changed ? result : value;
  }
  return value;
}

// node_modules/@a2ui/web_core/src/catalog/types.js
function createFunctionImplementation(api, execute) {
  return {
    name: api.name,
    returnType: api.returnType,
    schema: api.schema,
    allowedCallers: api.allowedCallers,
    requiresUserActivation: api.requiresUserActivation,
    description: api.description,
    execute
  };
}
var Catalog = class _Catalog {
  /**
   * Self-contained JSON Schema for validating payloads against this catalog.
   *
   * Built from the catalog's model and memoized: from the definition of each
   * entry `toJson` writes, or, for an entry defined in code without
   * specification annotations, from its Zod schema. Common
   * types references are localized and the referenced standard definitions
   * for the catalog's protocol version, and any of the catalog's `defs` an
   * entry references, are bundled into `$defs`, together with the component
   * envelope and the `anyComponent`/`anyFunction` unions. Use `toJson` for the
   * catalog document itself.
   *
   * Each read returns a fresh copy that the caller may modify.
   */
  get validationSchema() {
    if (!this.cachedValidationSchema) {
      this.cachedValidationSchema = generateValidationSchema(this);
    }
    return deepCopyJson(this.cachedValidationSchema);
  }
  /**
   * Dynamically reconstructs and memoizes the unified standard A2UI catalog JSON Schema document.
   *
   * @deprecated Use `validationSchema` for the bundled schema or `toJson` for
   *   the catalog document.
   */
  get catalogSchema() {
    return this.validationSchema;
  }
  /**
   * Returns the unbundled catalog document for this catalog, assembled from
   * its model and memoized.
   *
   * The document-level fields come from the catalog (`CatalogDocumentOptions`).
   * An entry `fromJson` loaded is written as its definition, with references
   * kept as written, unless it references a local definition the catalog
   * lacks. An entry defined in code whose schema is a specification model is
   * computed from its model in the layout of the catalog's protocol version,
   * with its mixins and the `defModels` written to `$defs`; any other entry
   * is generated from its Zod schema. Common types references are written
   * against `commonTypesUri`. The `anyComponent` and `anyFunction` unions are
   * computed from the entries. So for a catalog loaded with `fromJson`, the
   * result equals the loaded document (minus any entries that the document's
   * `anyComponent.oneOf` or `anyFunction.oneOf` unions omit, which `fromJson`
   * filters out on load), and for a built-in basic catalog, the published
   * document.
   *
   * @returns A fresh document that the caller may modify.
   */
  toJson() {
    if (!this.cachedDocument) {
      this.cachedDocument = serializeCatalogDocument(this);
    }
    return deepCopyJson(this.cachedDocument);
  }
  /**
   * Returns a catalog with this catalog's ID, version, theme and
   * document-level fields, and the given entries instead of its own.
   *
   * Definitions of `defs` that only entries left out referenced are dropped,
   * so `toJson` of the result does not carry, say, the mixin of a pruned
   * component. Catalog transformers use it to derive a catalog.
   *
   * @param components The components of the new catalog.
   * @param functions The functions of the new catalog.
   * @returns A new catalog.
   */
  withEntries(components, functions) {
    return this.rebuild(components, functions, {
      id: this.id,
      schemaId: this.schemaId,
      title: this.title,
      description: this.description,
      instructions: this.instructions
    });
  }
  /**
   * Returns a new catalog, under its own ID, built from entries of this one
   * and new entries.
   *
   * In `components` and `functions`, a name keeps this catalog's entry of
   * that name, and an entry adds it, or replaces this catalog's entry of the
   * same name. Leaving either list out keeps all of this catalog's entries of
   * that kind. The new catalog keeps this catalog's version, theme and the
   * document-level fields that describe its layout, and drops `defs` that only
   * entries left out referenced, as `withEntries` does.
   *
   * It does not keep this catalog's `$id`, title, description or
   * instructions, which describe this catalog: instructions often name its
   * ID and its components in their examples, so they would teach a model to
   * write messages for the wrong catalog.
   *
   * @returns A new catalog.
   * @throws {A2uiCatalogError} If a name is not an entry of this catalog.
   */
  derive(options) {
    const pick = (kind, own, wanted) => {
      if (wanted === void 0)
        return [...own.values()];
      return wanted.map((item) => {
        if (typeof item !== "string")
          return item;
        const entry = own.get(item);
        if (!entry) {
          throw new A2uiCatalogError(`Catalog '${this.id}' has no ${kind} '${item}' to derive catalog '${options.catalogId}' from.`);
        }
        return entry;
      });
    };
    return this.rebuild(pick("component", this.components, options.components), pick("function", this.functions, options.functions), { id: options.catalogId, instructions: options.instructions });
  }
  /**
   * Builds a catalog with this catalog's version, theme and layout, the given
   * entries and the given fields that describe the catalog.
   */
  rebuild(components, functions, described) {
    const componentsChanged = components.length !== this.components.size || components.some((c5) => this.components.get(c5.name) !== c5);
    const functionsChanged = functions.length !== this.functions.size || functions.some((f4) => this.functions.get(f4.name) !== f4);
    let baseDefs = this.defs;
    if (baseDefs && (componentsChanged || functionsChanged)) {
      const copy = { ...baseDefs };
      if (componentsChanged)
        delete copy["anyComponent"];
      if (functionsChanged)
        delete copy["anyFunction"];
      baseDefs = copy;
    }
    const unions = this.unions ? {
      anyComponent: this.unions.anyComponent || componentsChanged,
      anyFunction: this.unions.anyFunction || functionsChanged
    } : void 0;
    return new _Catalog(described.id, this.protocolVersion, components, functions, {
      themeSchema: this.themeSchema,
      instructions: described.instructions,
      schemaUri: this.schemaUri,
      schemaId: described.schemaId,
      title: described.title,
      description: described.description,
      declaredProtocolVersion: this.declaredProtocolVersion,
      declaresCatalogId: this.declaresCatalogId,
      declaresComponents: this.declaresComponents,
      functionsLayout: this.functionsLayout,
      unions,
      defs: retainedDefs(baseDefs, [...this.components.values(), ...this.functions.values()], [...components, ...functions], this.extensionFields),
      extensionFields: this.extensionFields,
      commonTypesUri: this.commonTypesUri,
      themeLocation: this.themeLocation,
      defModels: this.defModels
    });
  }
  /**
   * Lazily computed component reference map for child reference extraction and topology validation.
   */
  get componentRefMap() {
    if (!this._componentRefMap) {
      const options = isAtLeastVersion(this.protocolVersion, "1.0") ? V10_CHILD_REF_OPTIONS : isAtLeastVersion(this.protocolVersion, "0.9") ? V09_CHILD_REF_OPTIONS : V08_CHILD_REF_OPTIONS;
      this._componentRefMap = buildComponentRefMap(this, options);
    }
    return this._componentRefMap;
  }
  constructor(id, protocolVersionOrComponents, componentsOrFunctions, functionsOrThemeSchemaOrOptions, themeSchemaOrOptionsOrInstructions, instructions) {
    let protocolVersion;
    let components;
    let functions;
    let themeSchemaOrOptions;
    let resolvedInstructions;
    if (typeof protocolVersionOrComponents !== "string") {
      protocolVersion = "0.9";
      components = protocolVersionOrComponents ?? [];
      functions = componentsOrFunctions ?? [];
      themeSchemaOrOptions = functionsOrThemeSchemaOrOptions;
      resolvedInstructions = themeSchemaOrOptionsOrInstructions;
    } else {
      protocolVersion = protocolVersionOrComponents;
      components = componentsOrFunctions ?? [];
      functions = functionsOrThemeSchemaOrOptions ?? [];
      themeSchemaOrOptions = themeSchemaOrOptionsOrInstructions;
      resolvedInstructions = instructions;
    }
    if (!protocolVersion) {
      throw new A2uiCatalogError("protocolVersion must be provided.");
    }
    this.id = id;
    this.protocolVersion = protocolVersion;
    const compMap = /* @__PURE__ */ new Map();
    for (const comp of components) {
      compMap.set(comp.name, comp);
    }
    this.components = compMap;
    const funcMap = /* @__PURE__ */ new Map();
    for (const fn of functions) {
      funcMap.set(fn.name, fn);
    }
    this.functions = funcMap;
    const options = isZodSchema(themeSchemaOrOptions) ? { themeSchema: themeSchemaOrOptions } : themeSchemaOrOptions ?? {};
    this.themeSchema = options.themeSchema;
    this.instructions = options.instructions ?? resolvedInstructions;
    this.schemaUri = options.schemaUri;
    this.schemaId = options.schemaId;
    this.title = options.title;
    this.description = options.description;
    this.declaredProtocolVersion = options.declaredProtocolVersion;
    this.declaresCatalogId = options.declaresCatalogId ?? true;
    this.declaresComponents = options.declaresComponents ?? true;
    this.functionsLayout = options.functionsLayout;
    this.unions = options.unions ? Object.freeze({ ...options.unions }) : void 0;
    this.defs = frozenJson(options.defs);
    this.extensionFields = frozenJson(options.extensionFields);
    this.commonTypesUri = options.commonTypesUri;
    this.themeLocation = options.themeLocation;
    this.defModels = options.defModels ? Object.freeze([...options.defModels]) : void 0;
    this.invoker = createCatalogInvoker(this.id, this.protocolVersion, this.functions);
  }
  /**
   * Constructs a schema-only Catalog from an authored A2UI catalog document.
   *
   * Each entry is a `JsonComponentApi` or `JsonFunctionApi` whose model is
   * its definition as written, from which its Zod schema is derived on first
   * use, and the catalog keeps the document-level fields, so `toJson` returns
   * the document unchanged.
   *
   * @param json Raw catalog document or client capabilities inline catalog.
   * @param protocolVersion Protocol version of the catalog. When given, it
   *   takes precedence over the `protocolVersion` the document declares: it
   *   becomes the catalog's `protocolVersion` and selects the version's
   *   loading rules, while `toJson` still writes the version the document
   *   declares. When omitted, the document's version applies, and for a
   *   document that declares none, such as those published before v1.0,
   *   `DEFAULT_PROTOCOL_VERSION`.
   * @param catalogId Catalog ID to use instead of the one the document
   *   declares, or for a document that declares none, such as the v0.8 basic
   *   catalog. It becomes the catalog's `id`, and replaces the document's own
   *   `catalogId` in `toJson`; a document that declares none still has none
   *   in `toJson`.
   * @returns A new Catalog populated with component and function schemas.
   * @throws {Error} If the catalog ID is missing or not a string.
   * @throws {A2uiCatalogError} If a v1.0 catalog has a `$ref` to a document
   *   other than itself or `common_types.json`, such as another catalog. Such
   *   a reference is never fetched.
   */
  static fromJson(json, protocolVersion, catalogId) {
    const { id, protocolVersion: resolvedVersion, components, functions, ...options } = parseCatalogSchema(json, protocolVersion, catalogId);
    return new _Catalog(id, resolvedVersion, components, functions, options);
  }
  /**
   * Constructs a schema-only Catalog directly from a raw A2UI catalog schema.
   *
   * @deprecated Use `Catalog.fromJson` instead.
   * @param catalogSchema Raw catalog schema or client capabilities payload object.
   * @param protocolVersion Protocol version of the catalog, which takes
   *   precedence over the one the schema declares, as in `fromJson`.
   * @returns A new Catalog populated with component and function schemas.
   * @throws {Error} If the catalog ID is missing or not a string.
   */
  static fromSchema(catalogSchema, protocolVersion) {
    return _Catalog.fromJson(catalogSchema, protocolVersion);
  }
};
function isZodSchema(value) {
  return typeof value === "object" && value !== null && typeof value.safeParse === "function";
}
function createCatalogInvoker(catalogId, protocolVersion, functions) {
  return (name, rawArgs, ctx, abortSignal) => {
    const fn = functions.get(name) ?? (isAtLeastVersion(protocolVersion, SpecVersion.V1_0) && name === "@index" ? IndexImplementation : void 0);
    if (!fn) {
      throw new A2uiExpressionError(`Function not found in catalog '${catalogId}': ${name}`, name);
    }
    assertRendererCaller(fn);
    assertUserActivation(fn, ctx);
    const execute = fn.execute;
    if (typeof execute !== "function") {
      throw new A2uiExpressionError(`Function '${name}' in catalog '${catalogId}' is schema-only and has no implementation.`, name);
    }
    const parseAndExecute = (resolvedArgs) => {
      try {
        const args = isAtLeastVersion(protocolVersion, SpecVersion.V1_0) ? coerceValidationResults(fn.schema, resolvedArgs) : resolvedArgs;
        const safeArgs = fn.schema.parse(args);
        return execute.call(fn, safeArgs, ctx, abortSignal);
      } catch (e7) {
        if (e7?.name === "ZodError" || e7 instanceof external_exports.ZodError) {
          throw new A2uiExpressionError(`Validation failed for function '${name}': ${e7.message}`, name, e7.errors ?? e7.issues);
        }
        throw e7;
      }
    };
    const settledArgs = settleDeep(rawArgs);
    if (!isThenable(settledArgs)) {
      return parseAndExecute(settledArgs);
    }
    return Promise.resolve(settledArgs).then((resolvedArgs) => abortSignal?.aborted ? void 0 : parseAndExecute(resolvedArgs));
  };
}
function assertRendererCaller(fn) {
  if (fn.allowedCallers === "agentOnly") {
    throw new A2uiExpressionError(`Function '${fn.name}' cannot be called by renderer (allowedCallers is agentOnly).`, fn.name);
  }
}
function assertUserActivation(fn, ctx) {
  if (!fn.requiresUserActivation)
    return;
  const isPassive = Boolean(ctx?.isPassiveEvaluation);
  const isActivated = Boolean(ctx?.isUserActivated);
  const browserUnactivated = !isActivated && typeof navigator !== "undefined" && !(navigator.userActivation?.isActive ?? true);
  if (isPassive || browserUnactivated) {
    throw new A2uiExpressionError(`Function '${fn.name}' requires user activation and cannot be evaluated without an active user gesture.`, fn.name);
  }
}

// node_modules/@a2ui/web_core/src/validation/integrity-checker.js
var MAX_GLOBAL_DEPTH = 50;
var MAX_FUNC_CALL_DEPTH = 5;
var RELAXED_PATH_PATTERN = /^(?:(?:\/(?:[^~/]|~[01])*)*|(?:[^~/]|~[01])+(?:\/(?:[^~/]|~[01])*)*)$/;
function* extractPointers(val, currentPath, nestedKeys) {
  if (typeof val === "string") {
    yield [val, currentPath];
  } else if (Array.isArray(val)) {
    for (let idx = 0; idx < val.length; idx++) {
      const item = val[idx];
      const subPath = `${currentPath}[${idx}]`;
      yield* extractPointers(item, subPath, nestedKeys);
    }
  } else if (typeof val === "object" && val !== null) {
    const obj = val;
    if ("componentId" in obj && typeof obj.componentId === "string" && ("path" in obj || "dataBinding" in obj)) {
      yield [obj.componentId, `${currentPath}.componentId`];
    } else if (nestedKeys?.size) {
      for (const subKey of nestedKeys) {
        if (subKey in obj) {
          yield* extractPointers(obj[subKey], `${currentPath}.${subKey}`);
        }
      }
    } else {
      for (const [subKey, subVal] of Object.entries(obj)) {
        yield* extractPointers(subVal, `${currentPath}.${subKey}`);
      }
    }
  }
}
function getOrCreateRefMap(catalog) {
  return catalog.componentRefMap;
}
function* getComponentReferences(component, catalogOrRefMap) {
  if (!component || typeof component !== "object") {
    return;
  }
  const refFieldsMap = catalogOrRefMap instanceof Catalog ? getOrCreateRefMap(catalogOrRefMap) : catalogOrRefMap;
  const compVal = component.component;
  let compType = "";
  let props = component;
  if (typeof compVal === "string") {
    compType = compVal;
  } else if (typeof compVal === "object" && compVal !== null) {
    const compObj = compVal;
    compType = Object.keys(compObj)[0] ?? "";
    props = compObj[compType] ?? {};
  }
  if (!compType || typeof props !== "object" || props === null) {
    return;
  }
  const childRefs = refFieldsMap[compType];
  const singleRefs = childRefs ? childRefs.singleRefs : /* @__PURE__ */ new Set();
  const listRefs = childRefs ? childRefs.listRefs : /* @__PURE__ */ new Set();
  const nestedRefs = childRefs?.nestedRefs;
  for (const [key, value] of Object.entries(props)) {
    if (singleRefs.has(key) || listRefs.has(key)) {
      yield* extractPointers(value, key, nestedRefs?.[key]);
    }
  }
}
var STRICT_VALIDATION = Object.freeze({
  allowOrphanComponents: false,
  allowDanglingReferences: false,
  allowMissingRoot: false,
  allowUnknownElements: false
});
var RELAXED_VALIDATION = Object.freeze({
  allowOrphanComponents: true,
  allowDanglingReferences: true,
  allowMissingRoot: true,
  allowUnknownElements: true
});
function traverseRecursionAndPaths(item, globalDepth, funcDepth) {
  if (globalDepth > MAX_GLOBAL_DEPTH) {
    throw new A2uiRecursionError(`Global recursion limit exceeded: Depth > ${MAX_GLOBAL_DEPTH}`);
  }
  if (Array.isArray(item)) {
    for (const x3 of item) {
      traverseRecursionAndPaths(x3, globalDepth + 1, funcDepth);
    }
    return;
  }
  if (typeof item === "object" && item !== null) {
    const obj = item;
    for (const path of [obj.path, obj["@path"]]) {
      if (typeof path === "string" && !RELAXED_PATH_PATTERN.test(path)) {
        throw new A2uiValidationError(`Invalid path syntax: '${path}'`);
      }
    }
    const isFunctionCallWrapper = "functionCall" in obj && typeof obj.functionCall === "object" && obj.functionCall !== null;
    const isBareFunctionCall = ("call" in obj || "@call" in obj) && "args" in obj;
    if (isFunctionCallWrapper) {
      if (funcDepth >= MAX_FUNC_CALL_DEPTH) {
        throw new A2uiRecursionError(`Recursion limit exceeded: functionCall depth > ${MAX_FUNC_CALL_DEPTH}`);
      }
      for (const [k2, v3] of Object.entries(obj)) {
        if (k2 === "functionCall") {
          traverseRecursionAndPaths(v3, globalDepth + 1, funcDepth + 1);
        } else {
          traverseRecursionAndPaths(v3, globalDepth + 1, funcDepth);
        }
      }
    } else if (isBareFunctionCall) {
      if (funcDepth >= MAX_FUNC_CALL_DEPTH) {
        throw new A2uiRecursionError(`Recursion limit exceeded: functionCall depth > ${MAX_FUNC_CALL_DEPTH}`);
      }
      for (const [k2, v3] of Object.entries(obj)) {
        if (k2 === "args") {
          traverseRecursionAndPaths(v3, globalDepth + 1, funcDepth + 1);
        } else {
          traverseRecursionAndPaths(v3, globalDepth + 1, funcDepth);
        }
      }
    } else {
      for (const v3 of Object.values(obj)) {
        traverseRecursionAndPaths(v3, globalDepth + 1, funcDepth);
      }
    }
  }
}
function validateRecursionAndPaths(data) {
  traverseRecursionAndPaths(data, 0, 0);
}

// node_modules/@a2ui/web_core/src/state/surface-components-model.js
var SurfaceComponentsModel = class {
  /**
   * Initializes a new `SurfaceComponentsModel` instance.
   *
   * @param catalogOrRefMap Optional Catalog or precomputed ComponentRefMap used for schema-driven child reference extraction.
   */
  constructor(catalogOrRefMap) {
    this.components = /* @__PURE__ */ new Map();
    this._onCreated = new EventEmitter();
    this._onDeleted = new EventEmitter();
    this.onCreated = this._onCreated;
    this.onDeleted = this._onDeleted;
    if (catalogOrRefMap) {
      this.setCatalog(catalogOrRefMap);
    }
  }
  /**
   * Associates a catalog or reference map with this model for schema-driven child detection.
   *
   * @param catalogOrRefMap Catalog instance or ComponentRefMap.
   */
  setCatalog(catalogOrRefMap) {
    this.refMap = catalogOrRefMap instanceof Catalog ? catalogOrRefMap.componentRefMap : catalogOrRefMap;
  }
  /**
   * Retrieves a component by its ID.
   *
   * @param id The ID of the component to retrieve.
   * @returns The component model, or undefined if not found.
   */
  get(id) {
    return this.components.get(id);
  }
  /**
   * Returns a copy of the internal component map.
   *
   * @returns Map of component models keyed by component ID.
   */
  getAll() {
    return new Map(this.components);
  }
  /**
   * Checks whether a component with the specified ID exists in the model.
   *
   * @param id Component identifier to check.
   * @returns Whether the component exists in the model.
   */
  has(id) {
    return this.components.has(id);
  }
  /**
   * Returns an iterator over the component ID and model entries.
   */
  get entries() {
    return this.components.entries();
  }
  /**
   * Returns an iterator over all component IDs in the model.
   */
  get keys() {
    return this.components.keys();
  }
  /**
   * Returns an iterator over all component models in the model.
   */
  get values() {
    return this.components.values();
  }
  /**
   * Total number of components in the model.
   */
  get size() {
    return this.components.size;
  }
  /**
   * Returns a readonly map of all active components in the model.
   */
  get componentsMap() {
    return this.components;
  }
  /**
   * Adds a component to the model.
   * Throws an error if a component with the same ID already exists.
   *
   * @param component The component to add.
   */
  addComponent(component) {
    if (this.components.has(component.id)) {
      throw new A2uiStateError(`Component with id '${component.id}' already exists.`);
    }
    this.components.set(component.id, component);
    this._onCreated.emit(component);
  }
  /**
   * Adds a component to the model at a position in its order.
   *
   * The model holds its components in the order they were added, and iteration, topology
   * checks and their error messages follow it. This puts the component where `index` says
   * instead of last, as when an undo restores a component that an update replaced: the
   * replacement went last, the restored component goes back where it was. Inserting before
   * the last position rebuilds the order, so it takes time in proportion to the components
   * the model holds. Like `addComponent`, it fires `onCreated`.
   *
   * @param index How many components come before it. An index at or past the end adds it
   *     last.
   * @param component The component to add.
   * @throws {A2uiStateError} If a component with the same ID already exists.
   */
  insertComponent(index, component) {
    if (this.components.has(component.id)) {
      throw new A2uiStateError(`Component with id '${component.id}' already exists.`);
    }
    if (index < this.components.size) {
      const after = [...this.components].slice(Math.max(0, index));
      for (const [id] of after)
        this.components.delete(id);
      this.components.set(component.id, component);
      for (const [id, existing] of after)
        this.components.set(id, existing);
    } else {
      this.components.set(component.id, component);
    }
    this._onCreated.emit(component);
  }
  /**
   * Removes a component from the model by its ID.
   * Disposes of the component upon removal.
   *
   * @param id The ID of the component to remove.
   */
  removeComponent(id) {
    const component = this.components.get(id);
    if (component) {
      this.components.delete(id);
      component.dispose();
      this._onDeleted.emit(id);
    }
  }
  /**
   * Extracts child component references for a given component identifier.
   *
   * @param componentId The ID of the component whose children should be resolved.
   * @returns Array of tuples containing `[referencedId, propertyPath]`.
   */
  getChildReferences(componentId) {
    const comp = this.components.get(componentId);
    if (!comp)
      return [];
    const refMap = comp.catalog.components.size ? comp.catalog.componentRefMap : this.refMap ?? comp.catalog.componentRefMap;
    return Array.from(getComponentReferences(comp.componentTree, refMap));
  }
  /**
   * Returns array of referenced child component IDs for a given component identifier.
   *
   * @param componentId The ID of the component whose child IDs should be retrieved.
   * @returns Array of unique child component IDs.
   */
  getChildIds(componentId) {
    return this.getChildReferences(componentId).map(([refId]) => refId);
  }
  /**
   * Detects self-references, circular dependencies, and exceeds depth limits via DFS.
   *
   * @param options Configuration specifying root node, max depth, or allowing missing root.
   * @returns Set of visited component identifiers.
   * @throws {A2uiRecursionError} When self-reference, cycle, or max depth limit is exceeded.
   */
  detectCycles(options = {}) {
    const rootId = options.rootId ?? "root";
    const maxDepth = options.maxDepth ?? MAX_GLOBAL_DEPTH;
    const allowMissingRoot = options.allowMissingRoot ?? false;
    for (const comp of this.components.values()) {
      for (const [childId, fieldName] of this.getChildReferences(comp.id)) {
        if (childId === comp.id) {
          throw new A2uiRecursionError(`Circular reference detected: Component '${comp.id}' references itself in field '${fieldName}' (Self-reference detected)`);
        }
      }
    }
    const visited = /* @__PURE__ */ new Set();
    const recursionStack = /* @__PURE__ */ new Set();
    const dfs = (nodeId, depth) => {
      if (depth > maxDepth) {
        throw new A2uiRecursionError(`Global recursion limit exceeded: logical depth > ${maxDepth}`);
      }
      visited.add(nodeId);
      recursionStack.add(nodeId);
      const childIds = this.getChildIds(nodeId);
      for (const childId of childIds) {
        if (!this.components.has(childId)) {
          continue;
        }
        if (!visited.has(childId)) {
          dfs(childId, depth + 1);
        } else if (recursionStack.has(childId)) {
          throw new A2uiRecursionError(`Circular reference detected involving component '${childId}'`);
        }
      }
      recursionStack.delete(nodeId);
    };
    if (allowMissingRoot) {
      const sortedIds = Array.from(this.components.keys()).sort();
      for (const nodeId of sortedIds) {
        if (!visited.has(nodeId)) {
          dfs(nodeId, 0);
        }
      }
    } else {
      if (this.components.has(rootId)) {
        dfs(rootId, 0);
      }
    }
    return visited;
  }
  /**
   * Validates topology, completeness, and references across the component hierarchy.
   *
   * @param options Validation settings (allowMissingRoot, allowDanglingReferences, allowOrphanComponents).
   * @throws {A2uiIntegrityError} If missing root, dangling references, or orphan components exist.
   * @throws {A2uiRecursionError} If self-reference, cycle, or depth limit is detected.
   */
  validateTopology(options = {}) {
    if (this.components.size === 0)
      return;
    const rootId = options.rootId ?? "root";
    const allowMissingRoot = options.allowMissingRoot ?? false;
    const allowDanglingReferences = options.allowDanglingReferences ?? false;
    const allowOrphanComponents = options.allowOrphanComponents ?? false;
    if (!allowMissingRoot && !this.components.has(rootId)) {
      throw new A2uiIntegrityError(`Missing root component: No component has id='${rootId}'`);
    }
    if (!allowDanglingReferences) {
      for (const comp of this.components.values()) {
        for (const [childId, fieldName] of this.getChildReferences(comp.id)) {
          if (!this.components.has(childId)) {
            throw new A2uiIntegrityError(`Component '${comp.id}' references non-existent component '${childId}' in field '${fieldName}' (Dangling reference '${childId}' in component '${comp.id}')`);
          }
        }
      }
    }
    const visited = this.detectCycles({ rootId, allowMissingRoot, maxDepth: options.maxDepth });
    if (!allowOrphanComponents && !allowMissingRoot) {
      if (visited.size < this.components.size) {
        const orphans = Array.from(this.components.keys()).filter((id) => !visited.has(id)).sort();
        if (orphans.length > 0) {
          throw new A2uiIntegrityError(`Component '${orphans[0]}' is not reachable from '${rootId}' (Orphaned component '${orphans[0]}' is not reachable from root)`);
        }
      }
    }
  }
  /**
   * Performs non-throwing validation of references and topology across the component graph.
   * Returns an array containing the first validation error encountered, or an empty array if valid.
   *
   * @param options Validation options.
   * @returns Array of validation error objects.
   */
  validateReferences(options = {}) {
    const errors = [];
    try {
      this.validateTopology(options);
    } catch (err) {
      if (err instanceof A2uiValidationError) {
        errors.push(err);
      } else if (err instanceof Error) {
        errors.push(new A2uiValidationError(err.message));
      }
    }
    return errors;
  }
  /**
   * Disposes the model, clearing all components and update event emitters.
   */
  dispose() {
    const deletedIds = Array.from(this.components.keys());
    for (const component of this.components.values()) {
      component.dispose();
    }
    this.components.clear();
    for (const id of deletedIds) {
      this._onDeleted.emit(id);
    }
    this._onCreated.dispose();
    this._onDeleted.dispose();
  }
};

// node_modules/@a2ui/web_core/src/state/surface-model.js
function canonicalProtocolVersion(version) {
  if (!version)
    return void 0;
  const canonical = toCanonicalVersion(version);
  if (!canonical)
    return version;
  return isAtLeastVersion(canonical, "1.0") ? canonical : `v${canonical}`;
}
var SurfaceModel = class {
  constructor(id, defaultCatalog, availableCatalogsOrTheme = /* @__PURE__ */ new Map(), themeOrSendDataModel = {}, sendDataModelOrDataModel = false, dataModel, rootId = "root", metadata, protocolVersion) {
    this.id = id;
    this.defaultCatalog = defaultCatalog;
    this.rootId = rootId;
    this._onAction = new EventEmitter();
    this._onError = new EventEmitter();
    this._onWarning = new EventEmitter();
    this._onMetadataChanged = new EventEmitter();
    this.onAction = this._onAction;
    this.onError = this._onError;
    this.onWarning = this._onWarning;
    this.onMetadataChanged = this._onMetadataChanged;
    this._metadataSignal = signal(cloneMetadata(metadata));
    this.metadataSignal = computed(() => getValue(this._metadataSignal));
    this.protocolVersion = canonicalProtocolVersion(protocolVersion ?? defaultCatalog?.protocolVersion);
    let availableCatalogs;
    let theme = {};
    let sendDataModel = false;
    if (availableCatalogsOrTheme !== void 0 && availableCatalogsOrTheme !== null && !(availableCatalogsOrTheme instanceof Map)) {
      if (typeof availableCatalogsOrTheme === "object" && !Array.isArray(availableCatalogsOrTheme)) {
        theme = availableCatalogsOrTheme;
        sendDataModel = themeOrSendDataModel === true;
        dataModel = sendDataModelOrDataModel instanceof DataModel ? sendDataModelOrDataModel : void 0;
        availableCatalogs = void 0;
      } else {
        throw new TypeError("availableCatalogs must be a Map, or undefined/null. Note that availableCatalogs is now the 3rd argument to SurfaceModel and theme is the 4th argument.");
      }
    } else {
      availableCatalogs = availableCatalogsOrTheme;
      theme = themeOrSendDataModel ?? {};
      sendDataModel = Boolean(sendDataModelOrDataModel);
    }
    this.theme = theme;
    this.sendDataModel = sendDataModel;
    const catalogs = new Map(availableCatalogs ?? []);
    if (defaultCatalog?.id && !catalogs.has(defaultCatalog.id)) {
      catalogs.set(defaultCatalog.id, defaultCatalog);
    }
    this.availableCatalogs = catalogs;
    this.dataModel = dataModel ?? new DataModel({});
    this.componentsModel = new SurfaceComponentsModel(defaultCatalog);
  }
  /**
   * The surface's default catalog.
   *
   * @deprecated Use {@link defaultCatalog}. Renamed for symmetry with the other
   *   SDKs now that a surface can carry more than one catalog. This alias will
   *   be removed in a future release. Typed as always present because every
   *   pre-1.0 surface has a default catalog.
   */
  get catalog() {
    return this.defaultCatalog;
  }
  /**
   * Resolves the catalog that an item on this surface uses.
   *
   * An item that names a catalog uses that entry of {@link availableCatalogs};
   * one that names none uses the {@link defaultCatalog}. Both the message
   * processor, when validating a payload, and `DataContext`, when running a
   * function call, resolve catalogs through this method.
   *
   * @param catalogId Catalog the item names, or `undefined` when it names none.
   * @param subject Description of the item for error messages, such as
   *   `Function call 'formatDate'`.
   * @returns The catalog the item resolves to.
   * @throws {A2uiCatalogError} If the named catalog is not available on this
   *   surface, or if the item names none and the surface has no default
   *   catalog.
   */
  resolveCatalog(catalogId, subject = "Item") {
    return resolveSurfaceCatalog(this, catalogId, subject);
  }
  /**
   * Synchronous snapshot of the surface's metadata envelope.
   */
  get metadata() {
    return peekValue(this._metadataSignal);
  }
  /**
   * Replaces the surface's metadata envelope, validating UAX #31 extension
   * keys and emitting changes when structurally modified.
   */
  set metadata(metadata) {
    const next = cloneMetadata(metadata);
    if (metadataEquals(peekValue(this._metadataSignal), next)) {
      return;
    }
    setValue(this._metadataSignal, next);
    void this._onMetadataChanged.emit(next);
  }
  /**
   * Convenience view of `metadata.extensions`.
   */
  get extensions() {
    return peekValue(this._metadataSignal)?.extensions;
  }
  /**
   * Sets a single extension entry on the surface metadata.
   *
   * Passing `undefined` removes the extension key. Passing `null` stores
   * `null` as an explicit JSON value.
   *
   * @param key Extension identifier (must satisfy UAX #31).
   * @param value Extension payload value.
   */
  setExtension(key, value) {
    if (value === void 0) {
      this.removeExtension(key);
      return;
    }
    const current = peekValue(this._metadataSignal);
    const nextExtensions = {
      ...current?.extensions ?? {},
      [key]: value
    };
    this.metadata = {
      ...current ?? {},
      extensions: nextExtensions
    };
  }
  /**
   * Removes an extension entry by key, clearing `metadata` to `undefined` when
   * no metadata entries remain.
   *
   * @param key Extension identifier to remove.
   */
  removeExtension(key) {
    const current = peekValue(this._metadataSignal);
    const currentExtensions = current?.extensions;
    if (!currentExtensions || !(key in currentExtensions)) {
      return;
    }
    const nextExtensions = { ...currentExtensions };
    delete nextExtensions[key];
    const rest = { ...current };
    delete rest.extensions;
    if (Object.keys(nextExtensions).length > 0) {
      this.metadata = { ...rest, extensions: nextExtensions };
    } else if (Object.keys(rest).length > 0) {
      this.metadata = rest;
    } else {
      this.metadata = void 0;
    }
  }
  /**
   * Dispatches an action from this surface to registered listeners.
   *
   * Resolves event action payload details, propagates any explicit `catalogId`
   * and `metadata`, and emits an `ActionPayload` via `onAction`. Local function
   * actions (`functionCall`) are executed locally and are not emitted as agent actions.
   *
   * @param payload Action payload (name and context) to dispatch.
   * @param sourceComponentId Identifier of the component that triggered the action.
   * @param metadata Optional metadata envelope to attach to the outbound action.
   * @returns A promise that resolves once all registered listeners have processed the action.
   */
  async dispatchAction(payload, sourceComponentId, metadata) {
    if (!payload || typeof payload !== "object") {
      return;
    }
    const eventPayload = extractActionTarget(payload);
    if (!eventPayload) {
      return;
    }
    const name = eventPayload.name;
    if (!name || typeof name !== "string") {
      return;
    }
    const rawContext = eventPayload.context;
    const context = rawContext && typeof rawContext === "object" && !Array.isArray(rawContext) ? rawContext : {};
    const actionToDispatch = {
      name,
      surfaceId: this.id,
      sourceComponentId,
      timestamp: (/* @__PURE__ */ new Date()).toISOString(),
      context
    };
    const catalogId = eventPayload.catalogId ?? payload.catalogId;
    if (typeof catalogId === "string" && catalogId) {
      actionToDispatch.catalogId = catalogId;
    }
    if (typeof eventPayload.userMessage === "string" && eventPayload.userMessage.length > 0) {
      actionToDispatch.userMessage = eventPayload.userMessage;
    }
    const payloadMetadata = eventPayload.metadata ?? payload.metadata;
    const mergedMetadata = mergeActionMetadata(payloadMetadata, metadata);
    if (mergedMetadata !== void 0) {
      actionToDispatch.metadata = mergedMetadata;
    }
    await this._onAction.emit(actionToDispatch);
  }
  /**
   * Dispatches an error from this surface to registered listeners.
   *
   * @param error Error payload to dispatch, conforming to the renderer-to-agent schema.
   * @returns A promise that resolves once all registered listeners have processed the error.
   */
  async dispatchError(error) {
    await this._onError.emit({
      ...error,
      surfaceId: this.id
    });
  }
  /**
   * Dispatches a non-fatal warning from this surface to registered listeners.
   *
   * @param warning The warning payload to dispatch.
   * @returns Promise that resolves once listeners have handled the warning.
   */
  async dispatchWarning(warning) {
    await this._onWarning.emit({
      ...warning,
      surfaceId: this.id
    });
  }
  /**
   * Disposes the surface, data model, components, and event emitters.
   */
  dispose() {
    this.dataModel.dispose();
    this.componentsModel.dispose();
    this._onAction.dispose();
    this._onError.dispose();
    this._onWarning.dispose();
    this._onMetadataChanged.dispose();
  }
};
function extractActionTarget(payload) {
  if ("event" in payload && payload.event && typeof payload.event === "object") {
    return payload.event;
  }
  if ("name" in payload && typeof payload.name === "string") {
    return payload;
  }
  return null;
}

// node_modules/@a2ui/web_core/src/state/surface-group-model.js
var SurfaceGroupModel = class {
  constructor() {
    this.surfaces = /* @__PURE__ */ new Map();
    this.surfaceUnsubscribers = /* @__PURE__ */ new Map();
    this.surfaceErrorUnsubscribers = /* @__PURE__ */ new Map();
    this._onSurfaceCreated = new EventEmitter();
    this._onSurfaceDeleted = new EventEmitter();
    this._onAction = new EventEmitter();
    this._onError = new EventEmitter();
    this.onSurfaceCreated = this._onSurfaceCreated;
    this.onSurfaceDeleted = this._onSurfaceDeleted;
    this.onAction = this._onAction;
    this.onError = this._onError;
  }
  /**
   * Emits a group-level error payload to all `onError` subscribers.
   *
   * @param error Structured error payload to emit.
   */
  emitError(error) {
    this._onError.emit(error);
  }
  /**
   * Adds a surface to the group.
   *
   * Ignores if a surface with the same ID already exists.
   *
   * @param surface The surface model to add.
   */
  addSurface(surface) {
    if (this.surfaces.has(surface.id)) {
      console.warn(`Surface ${surface.id} already exists. Ignoring.`);
      return;
    }
    this.surfaces.set(surface.id, surface);
    const sub = surface.onAction.subscribe((action) => {
      void this._onAction.emit(action);
    });
    this.surfaceUnsubscribers.set(surface.id, sub);
    const errSub = surface.onError.subscribe((err) => {
      void this._onError.emit(err);
    });
    this.surfaceErrorUnsubscribers.set(surface.id, errSub);
    this._onSurfaceCreated.emit(surface);
  }
  /**
   * Removes a surface from the group by its ID.
   *
   * Disposes of the surface upon removal.
   *
   * @param id The ID of the surface to remove.
   */
  deleteSurface(id) {
    const surface = this.surfaces.get(id);
    if (surface) {
      const sub = this.surfaceUnsubscribers.get(id);
      if (sub) {
        sub.unsubscribe();
        this.surfaceUnsubscribers.delete(id);
      }
      const errSub = this.surfaceErrorUnsubscribers.get(id);
      if (errSub) {
        errSub.unsubscribe();
        this.surfaceErrorUnsubscribers.delete(id);
      }
      this.surfaces.delete(id);
      surface.dispose();
      this._onSurfaceDeleted.emit(id);
    }
  }
  /**
   * Retrieves a surface by its ID.
   *
   * @param id The ID of the surface to retrieve.
   * @returns The surface model, or undefined if not found.
   */
  getSurface(id) {
    return this.surfaces.get(id);
  }
  /**
   * Readonly map of all active surfaces keyed by surface ID.
   */
  get surfacesMap() {
    return this.surfaces;
  }
  /**
   * Disposes the group and all contained surfaces.
   */
  dispose() {
    for (const id of Array.from(this.surfaces.keys())) {
      this.deleteSurface(id);
    }
    this._onSurfaceCreated.dispose();
    this._onSurfaceDeleted.dispose();
    this._onAction.dispose();
    this._onError.dispose();
  }
};

// node_modules/@a2ui/web_core/src/state/component-model.js
var ComponentModel = class {
  /**
   * Initializes a new `ComponentModel` instance.
   *
   * @param id The unique identifier for this component.
   * @param type The component type name.
   * @param initialProperties The initial properties for the component.
   * @param catalog The catalog that defines this component's type. Omitting
   *   it is deprecated and kept only for pre-1.0 callers; pass the catalog.
   * @param metadata Optional component-level metadata envelope.
   */
  constructor(id, type, initialProperties, catalog, metadata) {
    this.id = id;
    this.type = type;
    this._onUpdated = new EventEmitter();
    this._onMetadataChanged = new EventEmitter();
    this.onUpdated = this._onUpdated;
    this.onMetadataChanged = this._onMetadataChanged;
    this._properties = initialProperties;
    this.catalog = catalog;
    this._metadataSignal = signal(cloneMetadata(metadata));
  }
  /**
   * Internal metadata signal used by `NodeResolver` to derive `ComponentNode.metadata`.
   *
   * @internal
   */
  get metadataSignal() {
    return this._metadataSignal;
  }
  /**
   * Synchronous snapshot of the component's metadata envelope.
   */
  get metadata() {
    return peekValue(this._metadataSignal);
  }
  /**
   * Replaces the component's metadata envelope, validating UAX #31 extension
   * keys and emitting changes when structurally modified.
   */
  set metadata(metadata) {
    const next = cloneMetadata(metadata);
    if (metadataEquals(peekValue(this._metadataSignal), next)) {
      return;
    }
    setValue(this._metadataSignal, next);
    void this._onMetadataChanged.emit(next);
  }
  /**
   * Convenience view of `metadata.extensions`.
   */
  get extensions() {
    return peekValue(this._metadataSignal)?.extensions;
  }
  /**
   * Sets a single extension entry on the component metadata.
   *
   * Passing `undefined` removes the extension key. Passing `null` stores
   * `null` as an explicit JSON value.
   *
   * @param key Extension identifier (must satisfy UAX #31).
   * @param value Extension payload value.
   */
  setExtension(key, value) {
    if (value === void 0) {
      this.removeExtension(key);
      return;
    }
    const current = peekValue(this._metadataSignal);
    const nextExtensions = {
      ...current?.extensions ?? {},
      [key]: value
    };
    this.metadata = {
      ...current ?? {},
      extensions: nextExtensions
    };
  }
  /**
   * Removes an extension entry by key, clearing `metadata` to `undefined` when
   * no metadata entries remain.
   *
   * @param key Extension identifier to remove.
   */
  removeExtension(key) {
    const current = peekValue(this._metadataSignal);
    const currentExtensions = current?.extensions;
    if (!currentExtensions || !(key in currentExtensions)) {
      return;
    }
    const nextExtensions = { ...currentExtensions };
    delete nextExtensions[key];
    const rest = { ...current };
    delete rest.extensions;
    if (Object.keys(nextExtensions).length > 0) {
      this.metadata = { ...rest, extensions: nextExtensions };
    } else if (Object.keys(rest).length > 0) {
      this.metadata = rest;
    } else {
      this.metadata = void 0;
    }
  }
  /**
   * Current properties of the component.
   */
  get properties() {
    return this._properties;
  }
  set properties(newProperties) {
    this._properties = newProperties;
    void this._onUpdated.emit(this);
  }
  /**
   * Atomically updates both properties and metadata before emitting update
   * notifications, ensuring synchronous listeners never observe torn state.
   *
   * @param newProperties Replacement component properties.
   * @param newMetadata Replacement component metadata envelope, or `undefined`.
   */
  applyUpdate(newProperties, newMetadata) {
    const nextMeta = cloneMetadata(newMetadata);
    const propsChanged = !jsonValueEquals(this._properties, newProperties);
    const metaChanged = !metadataEquals(peekValue(this._metadataSignal), nextMeta);
    if (propsChanged) {
      this._properties = newProperties;
    }
    if (metaChanged) {
      setValue(this._metadataSignal, nextMeta);
    }
    if (propsChanged) {
      void this._onUpdated.emit(this);
    }
    if (metaChanged) {
      void this._onMetadataChanged.emit(nextMeta);
    }
  }
  /**
   * Disposes the component model and its update event emitters.
   */
  dispose() {
    this._onUpdated.dispose();
    this._onMetadataChanged.dispose();
  }
  /**
   * Serialized JSON representation of the component definition.
   */
  get componentTree() {
    const metadata = this.metadata;
    return {
      ...this._properties,
      id: this.id,
      component: this.type,
      ...metadata !== void 0 ? { metadata } : {}
    };
  }
};

// node_modules/@a2ui/web_core/src/processing/message-error-mapper.js
var ACTION_ENVELOPE_KEYS = [
  "createSurface",
  "updateComponents",
  "updateDataModel",
  "deleteSurface",
  "callRendererFunction",
  "agentFunctionResponse",
  "beginRendering",
  "surfaceUpdate",
  "dataModelUpdate"
];
function isPlainRecord(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
function extractEnvelopeErrorContext(rawMessage, fallbackVersion) {
  if (!isPlainRecord(rawMessage)) {
    return { version: fallbackVersion };
  }
  const version = typeof rawMessage["version"] === "string" ? rawMessage["version"] : fallbackVersion;
  const actionType = ACTION_ENVELOPE_KEYS.find((key) => Object.hasOwn(rawMessage, key));
  if (!actionType) {
    return { version };
  }
  const isCallRendererFunction = actionType === "callRendererFunction";
  const payload = rawMessage[actionType];
  if (!isPlainRecord(payload)) {
    return { actionType, isCallRendererFunction, version };
  }
  const surfaceId = typeof payload["surfaceId"] === "string" ? payload["surfaceId"] : void 0;
  const functionCallId = typeof payload["functionCallId"] === "string" ? payload["functionCallId"] : void 0;
  return {
    surfaceId,
    functionCallId,
    actionType,
    isCallRendererFunction,
    version
  };
}
function stripActionPrefix(segments) {
  if (segments.length > 0 && ACTION_ENVELOPE_KEYS.includes(segments[0])) {
    return segments.slice(1);
  }
  return segments;
}
function normalizeJsonPointer(rawPath) {
  if (!rawPath)
    return void 0;
  if (rawPath.startsWith("/")) {
    return rawPath;
  }
  const converted = rawPath.replace(/\[(\d+)\]/g, "/$1").replace(/\./g, "/").replace(/^\/+/, "");
  if (!converted)
    return "/";
  const segments = stripActionPrefix(converted.split("/").filter(Boolean));
  return segments.length > 0 ? `/${segments.join("/")}` : void 0;
}
function collectLeafZodIssues(issues, actionType) {
  const leaves = [];
  for (const issue of issues) {
    if (issue.code === external_exports.ZodIssueCode.invalid_union && "unionErrors" in issue) {
      const unionIssues = issue.unionErrors.flatMap((u4) => u4.issues);
      leaves.push(...collectLeafZodIssues(unionIssues, actionType));
    } else {
      leaves.push(issue);
    }
  }
  if (!actionType)
    return leaves;
  const matching = leaves.filter((i7) => i7.path[0] === actionType && i7.path.length > 1);
  return matching.length > 0 ? matching : leaves;
}
function extractPathFromZodDetails(details, actionType) {
  let issues;
  if (details instanceof external_exports.ZodError) {
    issues = details.issues;
  } else if (Array.isArray(details) && details.length > 0) {
    const first = details[0];
    if (typeof first?.["path"] === "string") {
      return normalizeJsonPointer(first["path"]);
    }
    if (Array.isArray(first?.["path"])) {
      issues = details;
    }
  }
  if (!issues || issues.length === 0)
    return void 0;
  const selected = collectLeafZodIssues(issues, actionType)[0];
  if (!selected)
    return void 0;
  const segments = selected.path.map((s5) => String(s5).replace(/~/g, "~0").replace(/\//g, "~1"));
  if (selected.code === external_exports.ZodIssueCode.unrecognized_keys && "keys" in selected && Array.isArray(selected.keys) && selected.keys.length > 0) {
    segments.push(String(selected.keys[0]).replace(/~/g, "~0").replace(/\//g, "~1"));
  }
  const stripped = stripActionPrefix(segments);
  return stripped.length > 0 ? `/${stripped.join("/")}` : void 0;
}
function normalizeOutputVersion(version) {
  if (version === "v0.8" || version === "0.8")
    return "v0.8";
  if (version === "v0.9.1" || version === "0.9.1")
    return "v0.9.1";
  if (version && isAtLeastVersion(version, "1.0"))
    return "1.0";
  return "v0.9";
}
function resolveErrorCode(err, isV1) {
  if (err instanceof A2uiRecursionError)
    return "RECURSION_LIMIT_EXCEEDED";
  if (err instanceof A2uiIntegrityError || err instanceof A2uiStateError) {
    return "INTEGRITY_ERROR";
  }
  if (err instanceof A2uiCatalogError)
    return "CATALOG_NOT_FOUND";
  if (err instanceof A2uiDataError)
    return "DATA_ERROR";
  if (err instanceof A2uiExpressionError)
    return "EXPRESSION_ERROR";
  if (err instanceof A2uiValidationError) {
    if (err.code === "UNALLOWED_PARENT" || err.code === "UNALLOWED_CHILD") {
      return isV1 ? err.code : "VALIDATION_FAILED";
    }
    return "VALIDATION_FAILED";
  }
  if (err instanceof A2uiError)
    return err.code || "INTERNAL_ERROR";
  return "INTERNAL_ERROR";
}
function mapErrorToRendererMessage(err, context = {}) {
  const version = normalizeOutputVersion(context.version);
  const isV1 = version === "1.0";
  const a2uiErr = err instanceof A2uiError ? err : void 0;
  const message = err instanceof Error ? err.message : String(err);
  const functionCallId = a2uiErr?.functionCallId ?? context.functionCallId;
  const isRpcError = err instanceof A2uiRpcError || isV1 && (context.isCallRendererFunction || context.actionType === "callRendererFunction" || functionCallId !== void 0);
  if (isRpcError) {
    const rpcCode = err instanceof A2uiRpcError ? err.code : "INVALID_FUNCTION_CALL";
    if (functionCallId !== void 0) {
      return {
        version,
        error: {
          code: rpcCode,
          functionCallId,
          message
        }
      };
    }
    const surfaceId2 = a2uiErr?.surfaceId ?? context.surfaceId ?? "";
    return {
      version,
      error: {
        code: rpcCode,
        surfaceId: surfaceId2,
        message
      }
    };
  }
  const code = resolveErrorCode(err, isV1);
  const surfaceId = a2uiErr?.surfaceId ?? context.surfaceId ?? "";
  const rawPath = a2uiErr?.path ?? (err instanceof A2uiValidationError ? extractPathFromZodDetails(err.details, context.actionType) : void 0);
  let path = normalizeJsonPointer(rawPath);
  if (path === void 0 && (code === "VALIDATION_FAILED" || code === "UNALLOWED_PARENT" || code === "UNALLOWED_CHILD")) {
    path = "/";
  }
  return {
    version,
    error: {
      code,
      surfaceId,
      ...path !== void 0 ? { path } : {},
      message
    }
  };
}

// node_modules/@a2ui/web_core/src/state/component-graph-bound.js
var ComponentGraphBound = class _ComponentGraphBound {
  constructor() {
    this.children = /* @__PURE__ */ new Map();
    this.parents = /* @__PURE__ */ new Map();
    this.heights = /* @__PURE__ */ new Map();
    this.maxHeight = 0;
  }
  /**
   * Builds the bound of a graph.
   *
   * @param graph Each component id with the ids it references, held or not.
   * @returns The bound, or undefined when the graph holds a cycle, which a
   *     graph may hold and still pass a check that starts only at the root.
   */
  static of(graph) {
    const bound = new _ComponentGraphBound();
    for (const [id, children] of graph) {
      bound.setChildren(id, children);
    }
    const waiting = /* @__PURE__ */ new Set();
    for (const start of graph.keys()) {
      if (bound.heights.has(start))
        continue;
      const stack = [
        { id: start, childIndex: 0, height: 0 }
      ];
      waiting.add(start);
      while (stack.length > 0) {
        const frame = stack[stack.length - 1];
        const children = bound.children.get(frame.id) ?? [];
        let ready = true;
        while (frame.childIndex < children.length) {
          const child = children[frame.childIndex];
          frame.childIndex++;
          if (!graph.has(child))
            continue;
          const below = bound.heights.get(child);
          if (below === void 0) {
            if (waiting.has(child))
              return void 0;
            frame.childIndex--;
            stack.push({ id: child, childIndex: 0, height: 0 });
            waiting.add(child);
            ready = false;
            break;
          }
          if (below >= frame.height)
            frame.height = below + 1;
        }
        if (!ready)
          continue;
        bound.heights.set(frame.id, frame.height);
        if (frame.height > bound.maxHeight)
          bound.maxHeight = frame.height;
        waiting.delete(frame.id);
        stack.pop();
      }
    }
    return bound;
  }
  /** The ids the component `id` references, held or not, in order. */
  childrenOf(id) {
    return this.children.get(id) ?? [];
  }
  /** The components that reference `id`. */
  parentsOf(id) {
    return this.parents.get(id) ?? /* @__PURE__ */ new Set();
  }
  setChildren(id, children) {
    for (const child of this.children.get(id) ?? []) {
      this.parents.get(child)?.delete(id);
    }
    for (const child of children) {
      let parents = this.parents.get(child);
      if (!parents) {
        parents = /* @__PURE__ */ new Set();
        this.parents.set(child, parents);
      }
      parents.add(id);
    }
    this.children.set(id, children);
  }
  /**
   * Updates the bound to the graph with `batch` applied, and tells whether it
   * shows that graph has no self-reference, no cycle and no chain of more
   * than `limit` references.
   *
   * The references and parents are updated whatever the answer, so
   * `childrenOf` and `parentsOf` describe the graph with the batch applied.
   * False means the bound cannot show the graph passes, not that it fails,
   * and leaves the heights unusable.
   *
   * @param batch Each component of the batch with the ids it references.
   * @param limit The longest chain allowed.
   * @param held Whether the graph holds an id before the batch.
   */
  admits(batch, limit, held) {
    for (const [id, children] of batch) {
      this.setChildren(id, children);
    }
    if (this.maxHeight > limit)
      return false;
    for (const [id, children] of batch) {
      if (children.includes(id))
        return false;
    }
    const holds = (id) => batch.has(id) || held(id);
    const rising = [];
    for (const [id, children] of batch) {
      let height = 0;
      for (const child of children) {
        const below = this.heights.get(child) ?? 0;
        if (holds(child) && below >= height)
          height = below + 1;
      }
      this.heights.set(id, height);
      rising.push(id);
    }
    for (let next = 0; next < rising.length; next++) {
      const id = rising[next];
      const height = this.heights.get(id) ?? 0;
      if (height > limit)
        return false;
      if (height > this.maxHeight)
        this.maxHeight = height;
      for (const parent of this.parents.get(id) ?? []) {
        if ((this.heights.get(parent) ?? 0) <= height) {
          this.heights.set(parent, height + 1);
          rising.push(parent);
        }
      }
    }
    return true;
  }
};
var ComponentsWatcher = class _ComponentsWatcher {
  constructor() {
    this.count = 0;
    this.seenCatalogs = /* @__PURE__ */ new Set();
  }
  /** The watcher of `model`, which starts following it on first use. */
  static of(model) {
    let watcher = watchers.get(model);
    if (!watcher) {
      watcher = new _ComponentsWatcher();
      watcher.watch(model);
      watchers.set(model, watcher);
    }
    return watcher;
  }
  /** How many changes have been counted. */
  get changes() {
    return this.count;
  }
  /**
   * The catalogs of every component the model has held since the watcher
   * started, including components since removed.
   */
  get catalogs() {
    return this.seenCatalogs;
  }
  watch(model) {
    const updates = /* @__PURE__ */ new Map();
    const bump = () => {
      this.count++;
    };
    const follow = (component) => {
      this.seenCatalogs.add(component.catalog);
      updates.get(component.id)?.unsubscribe();
      updates.set(component.id, component.onUpdated.subscribe(bump));
    };
    for (const component of model.values) {
      follow(component);
    }
    model.onCreated.subscribe((component) => {
      this.count++;
      follow(component);
    });
    model.onDeleted.subscribe((id) => {
      this.count++;
      updates.get(id)?.unsubscribe();
      updates.delete(id);
    });
  }
};
var watchers = /* @__PURE__ */ new WeakMap();

// node_modules/@a2ui/web_core/src/state/validation-helpers.js
var catalogRules = /* @__PURE__ */ new WeakMap();
function validateComponentGraph(surface, newComponents, validationConfig, resolveCatalog, graphBounds) {
  const model = surface.componentsModel;
  const recorded = graphBounds.get(model);
  graphBounds.delete(model);
  if (!validationConfig || !defersGraphChecks(validationConfig)) {
    validateCompositionConstraints(surface, newComponents, resolveCatalog, validationConfig);
    validateCandidateTopology(surface, newComponents, resolveCatalog, validationConfig);
    return void 0;
  }
  const batch = batchGraph(surface, newComponents, resolveCatalog);
  const rootId = validationConfig.rootId ?? surface.rootId;
  if (batch && recorded && recorded.changes === ComponentsWatcher.of(model).changes && (validationConfig.allowMissingRoot || batch.children.has(rootId) || model.has(rootId))) {
    const { bound } = recorded;
    const previousChildren = /* @__PURE__ */ new Map();
    for (const id of batch.children.keys()) {
      previousChildren.set(id, bound.childrenOf(id));
    }
    const admitted = bound.admits(batch.children, validationConfig.maxDepth ?? MAX_GLOBAL_DEPTH, (id) => model.has(id));
    if (!compositionHolds(surface, batch, previousChildren, bound, validationConfig)) {
      validateCompositionConstraints(surface, newComponents, resolveCatalog, validationConfig);
    }
    if (admitted)
      return bound;
    const candidateGraph2 = validateCandidateTopology(surface, newComponents, resolveCatalog, validationConfig, true);
    return candidateGraph2 ? ComponentGraphBound.of(candidateGraph2) : void 0;
  }
  validateCompositionConstraints(surface, newComponents, resolveCatalog, validationConfig);
  const candidateGraph = validateCandidateTopology(surface, newComponents, resolveCatalog, validationConfig, true);
  return candidateGraph ? ComponentGraphBound.of(candidateGraph) : void 0;
}
function recordComponentGraphBound(model, bound, graphBounds) {
  graphBounds.set(model, { bound, changes: ComponentsWatcher.of(model).changes });
}
function defersGraphChecks(validationConfig) {
  return validationConfig !== void 0 && validationConfig.allowDanglingReferences === true && (validationConfig.allowOrphanComponents === true || validationConfig.allowMissingRoot === true);
}
function batchGraph(surface, newComponents, resolveCatalog) {
  const batch = { children: /* @__PURE__ */ new Map(), types: /* @__PURE__ */ new Map(), catalogs: /* @__PURE__ */ new Map() };
  for (const comp of newComponents) {
    const { id, component, ...properties } = comp;
    if (typeof id !== "string" || !id)
      continue;
    const catalog = resolveCatalog(comp, surface);
    delete properties.catalogId;
    delete properties.metadata;
    const type = (typeof component === "string" ? component : surface.componentsModel.get(id)?.type) || "";
    if (!type || catalog.components.size === 0)
      return void 0;
    const children = Array.from(getComponentReferences({ ...properties, id, component: type }, catalog.componentRefMap), ([childId]) => childId);
    batch.children.set(id, children);
    batch.types.set(id, type);
    batch.catalogs.set(id, catalog);
  }
  return batch;
}
function compositionHolds(surface, batch, previousChildren, bound, validationConfig) {
  if (!hasCompositionRules(surface, batch.catalogs.values(), validationConfig))
    return true;
  const rootId = compositionRootId(surface, validationConfig);
  const model = surface.componentsModel;
  const typeMap = {
    get: (id) => batch.types.get(id) ?? model.get(id)?.type
  };
  const moved = /* @__PURE__ */ new Set();
  const holding = /* @__PURE__ */ new Map();
  for (const [id, children] of batch.children) {
    for (const child of children) {
      if (!batch.children.has(child))
        moved.add(child);
    }
    for (const child of previousChildren.get(id) ?? []) {
      if (!batch.children.has(child))
        moved.add(child);
    }
    for (const parent of bound.parentsOf(id)) {
      if (batch.children.has(parent))
        continue;
      const held = holding.get(parent);
      if (held) {
        held.push(id);
      } else {
        holding.set(parent, [id]);
      }
    }
  }
  const componentApiOf = (id) => {
    const componentType = typeMap.get(id);
    if (!componentType)
      return void 0;
    const catalog = batch.catalogs.get(id) ?? model.get(id)?.catalog;
    const componentApi = catalog?.components.get(componentType);
    return componentApi ? { componentType, componentApi } : void 0;
  };
  const checkParents = (id) => {
    const entry = componentApiOf(id);
    if (!entry)
      return;
    const { componentType, componentApi } = entry;
    if (componentApi.allowedParents && componentApi.allowedParents.length > 0) {
      const parents = Array.from(bound.parentsOf(id), (parentId) => ({
        parentId,
        parentType: typeMap.get(parentId) || "Unknown"
      }));
      validateParentConstraints(id, componentType, componentApi.allowedParents, parents, rootId);
    }
  };
  const checkChildren = (id, children) => {
    const entry = componentApiOf(id);
    if (!entry)
      return;
    const { componentType, componentApi } = entry;
    if (componentApi.allowedChildren && componentApi.allowedChildren.length > 0) {
      validateChildConstraints(id, componentType, componentApi.allowedChildren, children, typeMap);
    }
  };
  try {
    for (const id of batch.children.keys()) {
      checkParents(id);
      checkChildren(id, bound.childrenOf(id));
    }
    for (const id of moved)
      checkParents(id);
    for (const [id, held] of holding)
      checkChildren(id, held);
  } catch {
    return false;
  }
  return true;
}
function hasCompositionRules(surface, batchCatalogs, validationConfig) {
  const declaresRules = (catalog) => {
    let rules = catalogRules.get(catalog);
    if (rules === void 0) {
      rules = Array.from(catalog.components.values()).some((api) => api.allowedParents !== void 0 && api.allowedParents.length > 0 || api.allowedChildren !== void 0 && api.allowedChildren.length > 0);
      catalogRules.set(catalog, rules);
    }
    return rules;
  };
  for (const catalog of batchCatalogs) {
    if (declaresRules(catalog))
      return true;
  }
  if (defersGraphChecks(validationConfig)) {
    for (const catalog of ComponentsWatcher.of(surface.componentsModel).catalogs) {
      if (declaresRules(catalog))
        return true;
    }
    return false;
  }
  const surfaceCatalogs = new Set(surface.availableCatalogs.values());
  if (surface.defaultCatalog)
    surfaceCatalogs.add(surface.defaultCatalog);
  for (const catalog of surfaceCatalogs) {
    if (declaresRules(catalog))
      return true;
  }
  for (const component of surface.componentsModel.values) {
    if (!surfaceCatalogs.has(component.catalog) && declaresRules(component.catalog)) {
      return true;
    }
  }
  return false;
}
function compositionRootId(surface, validationConfig) {
  return validationConfig?.rootId ?? surface.rootId ?? "root";
}
function validateParentConstraints(id, componentType, allowedParents, parents, rootId, surfaceId, compIndex, compIndexMap) {
  if (id === rootId && !allowedParents.includes("Surface")) {
    const path = compIndex !== void 0 ? `/components/${compIndex}` : "/components";
    const message = `Component '${id}' (${componentType}) cannot be placed under parent 'Surface' (Surface). Allowed parents: ${JSON.stringify(allowedParents)}.`;
    const details = [
      {
        path,
        code: "UNALLOWED_PARENT",
        message,
        ...surfaceId !== void 0 ? { surfaceId } : {}
      }
    ];
    throw new A2uiValidationError(message, details, "UNALLOWED_PARENT", { path, surfaceId });
  }
  for (const parentInfo of parents ?? []) {
    if (!allowedParents.includes(parentInfo.parentType)) {
      const offendingIndex = compIndex ?? compIndexMap?.get(parentInfo.parentId);
      const path = offendingIndex !== void 0 ? `/components/${offendingIndex}` : "/components";
      const message = `Component '${id}' (${componentType}) cannot be placed under parent '${parentInfo.parentId}' (${parentInfo.parentType || "unknown"}). Allowed parents: ${JSON.stringify(allowedParents)}.`;
      const details = [
        {
          path,
          code: "UNALLOWED_PARENT",
          message,
          ...surfaceId !== void 0 ? { surfaceId } : {}
        }
      ];
      throw new A2uiValidationError(message, details, "UNALLOWED_PARENT", { path, surfaceId });
    }
  }
}
function validateChildConstraints(id, componentType, allowedChildren, children, typeMap, surfaceId, compIndex, childPathMap, compIndexMap) {
  for (const childId of children) {
    const childType = typeMap.get(childId);
    if (childType && !allowedChildren.includes(childType)) {
      let path;
      if (compIndex !== void 0) {
        const rawField = childPathMap?.get(childId) ?? "children";
        const normalizedField = rawField.replace(/\[(\d+)\]/g, "/$1").replace(/\./g, "/").replace(/^\/+/, "");
        path = `/components/${compIndex}${normalizedField ? `/${normalizedField}` : ""}`;
      } else {
        const offendingIndex = compIndexMap?.get(childId);
        path = offendingIndex !== void 0 ? `/components/${offendingIndex}` : "/components";
      }
      const message = `Container '${id}' (${componentType}) cannot contain child '${childId}' (${childType}). Allowed children: ${JSON.stringify(allowedChildren)}.`;
      const details = [
        {
          path,
          code: "UNALLOWED_CHILD",
          message,
          ...surfaceId !== void 0 ? { surfaceId } : {}
        }
      ];
      throw new A2uiValidationError(message, details, "UNALLOWED_CHILD", { path, surfaceId });
    }
  }
}
function buildCompositionTopology(surface, newComponents, resolveCatalog) {
  const typeMap = /* @__PURE__ */ new Map();
  const compCatalogMap = /* @__PURE__ */ new Map();
  const childMap = /* @__PURE__ */ new Map();
  const childPathMap = /* @__PURE__ */ new Map();
  const compIndexMap = /* @__PURE__ */ new Map();
  for (const [id, model] of surface.componentsModel.entries) {
    typeMap.set(id, model.type);
    compCatalogMap.set(id, model.catalog);
    const refs = surface.componentsModel.getChildReferences(id);
    if (refs.length > 0) {
      childMap.set(id, refs.map(([childId]) => childId));
      childPathMap.set(id, new Map(refs));
    }
  }
  for (let i7 = 0; i7 < newComponents.length; i7++) {
    const comp = newComponents[i7];
    const { id, component, ...props } = comp;
    if (typeof id !== "string")
      continue;
    compIndexMap.set(id, i7);
    const compCatalog = resolveCatalog(comp, surface);
    compCatalogMap.set(id, compCatalog);
    const existing = surface.componentsModel.get(id);
    const compType = (typeof component === "string" ? component : existing?.type) ?? "";
    if (compType) {
      typeMap.set(id, compType);
    }
    const compDef = { id, component: compType, ...props };
    const refs = Array.from(getComponentReferences(compDef, compCatalog));
    if (refs.length > 0) {
      childMap.set(id, refs.map(([childId]) => childId));
      childPathMap.set(id, new Map(refs));
    } else {
      childMap.delete(id);
      childPathMap.delete(id);
    }
  }
  const parentMap = /* @__PURE__ */ new Map();
  for (const [parentId, children] of childMap.entries()) {
    const parentType = typeMap.get(parentId) || "Unknown";
    for (const childId of children) {
      const parents = parentMap.get(childId) ?? [];
      parents.push({ parentId, parentType });
      parentMap.set(childId, parents);
    }
  }
  return { typeMap, compCatalogMap, childMap, childPathMap, parentMap, compIndexMap };
}
function validateCompositionConstraints(surface, newComponents, resolveCatalog, validationConfig) {
  const batchCatalogs = newComponents.filter((comp) => typeof comp.id === "string").map((comp) => resolveCatalog(comp, surface));
  if (!hasCompositionRules(surface, batchCatalogs, validationConfig))
    return;
  const { typeMap, compCatalogMap, childMap, childPathMap, parentMap, compIndexMap } = buildCompositionTopology(surface, newComponents, resolveCatalog);
  const rootId = compositionRootId(surface, validationConfig);
  for (const [id, componentType] of typeMap.entries()) {
    const compCatalog = compCatalogMap.get(id);
    const componentApi = compCatalog?.components.get(componentType);
    if (!componentApi)
      continue;
    if (componentApi.allowedParents && componentApi.allowedParents.length > 0) {
      validateParentConstraints(id, componentType, componentApi.allowedParents, parentMap.get(id), rootId, surface.id, compIndexMap.get(id), compIndexMap);
    }
    if (componentApi.allowedChildren && componentApi.allowedChildren.length > 0) {
      validateChildConstraints(id, componentType, componentApi.allowedChildren, childMap.get(id) || [], typeMap, surface.id, compIndexMap.get(id), childPathMap.get(id), compIndexMap);
    }
  }
}
function validateCandidateTopology(surface, newComponents, resolveCatalog, validationConfig, collectGraph = false) {
  if (!validationConfig)
    return void 0;
  const candidateModel = new SurfaceComponentsModel(surface.defaultCatalog);
  for (const [id, comp] of surface.componentsModel.entries) {
    candidateModel.addComponent(new ComponentModel(id, comp.type, comp.properties, comp.catalog));
  }
  for (const comp of newComponents) {
    const { id, component, ...properties } = comp;
    if (typeof id !== "string" || !id)
      continue;
    const targetCatalog = resolveCatalog(comp, surface);
    delete properties.catalogId;
    delete properties.metadata;
    const existing = candidateModel.get(id);
    const componentType = (typeof component === "string" ? component : existing?.type) || "";
    if (!componentType)
      continue;
    if (existing) {
      if (componentType !== existing.type || existing.catalog.id !== targetCatalog.id) {
        candidateModel.removeComponent(id);
        candidateModel.addComponent(new ComponentModel(id, componentType, properties, targetCatalog));
      } else {
        existing.properties = properties;
      }
    } else {
      candidateModel.addComponent(new ComponentModel(id, componentType, properties, targetCatalog));
    }
  }
  candidateModel.validateTopology({
    ...validationConfig,
    rootId: validationConfig.rootId ?? surface.rootId
  });
  if (!collectGraph)
    return void 0;
  const graph = /* @__PURE__ */ new Map();
  for (const id of candidateModel.keys) {
    graph.set(id, candidateModel.getChildIds(id));
  }
  return graph;
}

// node_modules/@a2ui/web_core/src/processing/format-zod-issue.js
function formatZodIssue(err) {
  const path = err.path.join(".") || "root";
  switch (err.code) {
    case external_exports.ZodIssueCode.invalid_union: {
      const unionIssues = err.unionErrors?.flatMap((uErr) => uErr.issues);
      if (unionIssues && unionIssues.length > 0) {
        return unionIssues.map(formatZodIssue).join("; ");
      }
      return `${path}: Invalid union`;
    }
    case external_exports.ZodIssueCode.unrecognized_keys: {
      const keysStr = err.keys.map((k2) => `'${k2}'`).join(", ");
      return `${path}: Unrecognized key(s) in object: ${keysStr}`;
    }
    case external_exports.ZodIssueCode.invalid_enum_value: {
      const issue = err;
      const optionsStr = issue.options.map((o7) => String(o7)).join(" | ");
      return `${path}: Invalid enum value. Expected ${optionsStr}, received '${String(issue.received)}'`;
    }
    case external_exports.ZodIssueCode.invalid_type: {
      const issue = err;
      return `${path}: Expected ${issue.expected}, received ${issue.received}`;
    }
    case external_exports.ZodIssueCode.custom:
      return `${path}: ${err.message}`;
    default:
      return err.message ? `${path}: ${err.message}` : `${path}: Validation error (${err.code})`;
  }
}

// node_modules/@a2ui/web_core/src/processing/adapters/base.js
var DEFAULT_CATALOG_COMPATIBILITY = {
  "0.8": /* @__PURE__ */ new Set(["0.8"]),
  "0.9": /* @__PURE__ */ new Set(["0.9", "0.9.1"]),
  "0.9.1": /* @__PURE__ */ new Set(["0.9.1", "0.9"]),
  "1.0": /* @__PURE__ */ new Set(["1.0"])
};
function isCatalogVersionCompatible(catalogVersion, messageVersion, compatibilityMap = DEFAULT_CATALOG_COMPATIBILITY) {
  if (!catalogVersion || !messageVersion) {
    return false;
  }
  const catalogCanonical = toCanonicalVersion(catalogVersion);
  const messageCanonical = toCanonicalVersion(messageVersion);
  if (catalogCanonical && messageCanonical) {
    if (catalogCanonical === messageCanonical) {
      return true;
    }
    const compatible = compatibilityMap[messageCanonical];
    if (compatible && compatible.has(catalogCanonical)) {
      return true;
    }
    const catalogSemver = toSemVer(catalogVersion);
    const messageSemver = toSemVer(messageVersion);
    if (catalogSemver && messageSemver && catalogSemver.major >= 1 && catalogSemver.major === messageSemver.major) {
      return true;
    }
    return false;
  }
  if (typeof catalogVersion !== "string" || typeof messageVersion !== "string") {
    return false;
  }
  const normalizedCatalog = normalizeVersionString(catalogVersion);
  const normalizedMessage = normalizeVersionString(messageVersion);
  return normalizedCatalog.length > 0 && normalizedCatalog === normalizedMessage;
}
var globalKnownActionsProvider;
function registerKnownActionsProvider(provider) {
  globalKnownActionsProvider = provider;
}
function validateActionSurfaceIds(msgObj, actionKeys) {
  for (const key of actionKeys) {
    const actionVal = msgObj[key];
    if (actionVal && typeof actionVal === "object") {
      const actionObj = actionVal;
      if ("surfaceId" in actionObj && actionObj.surfaceId !== void 0 && typeof actionObj.surfaceId !== "string") {
        const msg = "surfaceId must be a string";
        throw new A2uiValidationError(msg, [{ path: `/${key}/surfaceId`, code: "type_mismatch", message: msg }], "VALIDATION_ERROR", { path: "/surfaceId" });
      }
    }
  }
}
var BaseVersionAdapter = class {
  /** Canonical catalog protocol versions compatible with this adapter. */
  get compatibleCatalogVersions() {
    const canonical = toCanonicalVersion(this.version);
    return canonical ? /* @__PURE__ */ new Set([canonical]) : /* @__PURE__ */ new Set();
  }
  /** Set of valid message action keys supported by this protocol version. */
  get validActions() {
    return new Set(this.getNativeActionKeys());
  }
  /** Checks if a catalog protocol version is compatible with this adapter. */
  isCatalogCompatible(catalogVersion) {
    if (!catalogVersion) {
      return false;
    }
    return isCatalogVersionCompatible(catalogVersion, this.version);
  }
  extractOperations(payload) {
    if (!payload || typeof payload !== "object")
      return [];
    if (Array.isArray(payload)) {
      return payload.flatMap((item) => this.extractOperations(item));
    }
    const msgObj = payload;
    if (Array.isArray(msgObj.messages)) {
      return this.extractOperations(msgObj.messages);
    }
    const nativeActionKeys = this.getNativeActionKeys();
    validateActionSurfaceIds(msgObj, nativeActionKeys);
    const presentNativeKeys = nativeActionKeys.filter((k2) => k2 in msgObj).sort();
    if (presentNativeKeys.length > 1) {
      throw new A2uiValidationError(`Message contains multiple conflicting update actions: ${presentNativeKeys.join(", ")}.`);
    }
    if (presentNativeKeys.length === 0) {
      const allKnown = globalKnownActionsProvider ? globalKnownActionsProvider() : new Set(nativeActionKeys);
      const otherAction = Object.keys(msgObj).find((k2) => allKnown.has(k2) && !nativeActionKeys.includes(k2));
      const sortedAllowed = nativeActionKeys.slice().sort().join(", ");
      if (otherAction) {
        const msg = `Invalid ${this.version} message: action '${otherAction}' is not supported in protocol version ${this.version}. Allowed actions: ${sortedAllowed}.`;
        throw new A2uiValidationError(msg, [{ path: "/version", code: "invalid_value", message: msg }], "VALIDATION_ERROR", { path: "/version" });
      }
      throw new A2uiValidationError(`Invalid ${this.version} message: message must contain exactly one update action: ${sortedAllowed}.`);
    }
    const preparedPayload = this.preparePayloadForValidation(msgObj);
    const parseResult = this.schema.safeParse(preparedPayload);
    if (!parseResult.success) {
      const formattedErrors = parseResult.error.errors.map(formatZodIssue).join("; ");
      const envCtx = extractEnvelopeErrorContext(msgObj, this.version);
      const preliminaryErr = new A2uiValidationError(`Invalid ${this.version} message: ${formattedErrors}`, parseResult.error, "VALIDATION_ERROR");
      const mapped = mapErrorToRendererMessage(preliminaryErr, envCtx);
      preliminaryErr.path = mapped.error.path;
      preliminaryErr.surfaceId = mapped.error.surfaceId || void 0;
      preliminaryErr.functionCallId = mapped.error.functionCallId;
      throw preliminaryErr;
    }
    return this.extractOperationsFromObject(msgObj);
  }
  /**
   * Normalizes the message object before running schema validation.
   *
   * Defaults to attaching `version` if not present.
   *
   * @param msgObj Raw message object.
   * @returns Normalized message payload for schema validation.
   */
  preparePayloadForValidation(msgObj) {
    return "version" in msgObj ? msgObj : { version: this.version, ...msgObj };
  }
};

// node_modules/@a2ui/web_core/src/v0_8/schema/common-types.js
var exactlyOneKey = (val, ctx) => {
  const keys = Object.keys(val).filter((k2) => val[k2] !== void 0);
  if (keys.length !== 1) {
    ctx.addIssue({
      code: external_exports.ZodIssueCode.custom,
      message: `Must define exactly one property, found ${keys.length} (${keys.join(", ")}).`
    });
  }
};
var StringValueSchema = external_exports.object({
  path: external_exports.string().optional(),
  literalString: external_exports.string().optional(),
  literal: external_exports.string().optional()
}).strict().superRefine(exactlyOneKey);
var DataValueMapItemSchema = external_exports.lazy(() => external_exports.object({
  key: external_exports.string(),
  valueString: external_exports.string().optional(),
  valueNumber: external_exports.number().optional(),
  valueBoolean: external_exports.boolean().optional(),
  valueMap: external_exports.array(DataValueMapItemSchema).optional()
}).strict().superRefine((val, ctx) => {
  let count = 0;
  if (val.valueString !== void 0)
    count++;
  if (val.valueNumber !== void 0)
    count++;
  if (val.valueBoolean !== void 0)
    count++;
  if (val.valueMap !== void 0)
    count++;
  if (count !== 1) {
    ctx.addIssue({
      code: external_exports.ZodIssueCode.custom,
      message: `Value map item must have exactly one value property (valueString, valueNumber, valueBoolean, valueMap), found ${count}.`
    });
  }
}));
function createDataValueSchema(options = {}) {
  const maxDepth = options.maxDepth ?? 5;
  return DataValueMapItemSchema.superRefine((val, ctx) => {
    const checkDepth = (v3, currentDepth) => {
      if (currentDepth > maxDepth) {
        ctx.addIssue({
          code: external_exports.ZodIssueCode.custom,
          message: `valueMap recursion exceeded maximum depth of ${maxDepth}.`
        });
        return;
      }
      if (v3.valueMap && Array.isArray(v3.valueMap)) {
        for (const item of v3.valueMap) {
          checkDepth(item, currentDepth + 1);
        }
      }
    };
    checkDepth(val, 1);
  });
}
var DataValueSchema = createDataValueSchema();
var NumberValueSchema = external_exports.object({
  path: external_exports.string().optional(),
  literalNumber: external_exports.number().optional(),
  literal: external_exports.number().optional()
}).strict().superRefine(exactlyOneKey);
var BooleanValueSchema = external_exports.object({
  path: external_exports.string().optional(),
  literalBoolean: external_exports.boolean().optional(),
  literal: external_exports.boolean().optional()
}).strict().superRefine(exactlyOneKey);
var ActionSchema3 = external_exports.object({
  name: external_exports.string().describe("A unique name identifying the action (e.g., 'submitForm')."),
  context: external_exports.array(external_exports.object({
    key: external_exports.string(),
    value: external_exports.object({
      path: external_exports.string().describe("A data binding reference to a location in the data model (e.g., '/user/name').").optional(),
      literalString: external_exports.string().describe("A fixed, hardcoded string value.").optional(),
      literalNumber: external_exports.number().optional(),
      literalBoolean: external_exports.boolean().optional()
    }).describe("The dynamic value. Define EXACTLY ONE of the nested properties.").strict().superRefine(exactlyOneKey)
  })).describe("A key-value map of data bindings to be resolved when the action is triggered.").optional()
});
var TextSchema = external_exports.object({
  text: StringValueSchema,
  usageHint: external_exports.enum(["h1", "h2", "h3", "h4", "h5", "caption", "body"]).optional()
});
var ImageSchema = external_exports.object({
  url: StringValueSchema,
  usageHint: external_exports.enum(["icon", "avatar", "smallFeature", "mediumFeature", "largeFeature", "header"]).optional(),
  fit: external_exports.enum(["contain", "cover", "fill", "none", "scale-down"]).optional(),
  altText: StringValueSchema.optional()
});
var IconSchema = external_exports.object({
  name: StringValueSchema
});
var VideoSchema = external_exports.object({
  url: StringValueSchema
});
var AudioPlayerSchema = external_exports.object({
  url: StringValueSchema,
  description: StringValueSchema.optional().describe("A label, title, or placeholder text.")
});
var TabsSchema = external_exports.object({
  tabItems: external_exports.array(external_exports.object({
    title: external_exports.object({
      path: external_exports.string().describe("A data binding reference to a location in the data model (e.g., '/user/name').").optional(),
      literalString: external_exports.string().describe("A fixed, hardcoded string value.").optional()
    }),
    child: external_exports.string().describe("A reference to a component instance by its unique ID.")
  }).strict().superRefine((val, ctx) => {
    if (!val.title) {
      ctx.addIssue({
        code: external_exports.ZodIssueCode.custom,
        message: "Tab item is missing 'title'."
      });
    }
    if (!val.child) {
      ctx.addIssue({
        code: external_exports.ZodIssueCode.custom,
        message: "Tab item is missing 'child'."
      });
    }
    if (val.title) {
      exactlyOneKey(val.title, ctx);
    }
  })).describe("A list of tabs, each with a title and a child component ID.")
});
var DividerSchema = external_exports.object({
  axis: external_exports.enum(["horizontal", "vertical"]).optional().describe("The orientation."),
  color: external_exports.string().optional().describe("The color of the divider (e.g., hex code or semantic name)."),
  thickness: external_exports.number().optional().describe("The thickness of the divider.")
});
var ModalSchema = external_exports.object({
  entryPointChild: external_exports.string().describe("The ID of the component (e.g., a button) that triggers the modal."),
  contentChild: external_exports.string().describe("The ID of the component to display as the modal's content.")
});
var ButtonSchema = external_exports.object({
  child: external_exports.string().describe("The ID of the component to display as the button's content."),
  action: ActionSchema3.describe("Represents a user-initiated action."),
  primary: external_exports.boolean().optional().describe("Indicates if this button should be styled as the primary action.")
});
var CheckboxSchema = external_exports.object({
  label: StringValueSchema,
  value: external_exports.object({
    path: external_exports.string().describe("A data binding reference to a location in the data model (e.g., '/user/name').").optional(),
    literalBoolean: external_exports.boolean().optional()
  }).strict().superRefine(exactlyOneKey)
});
var TextFieldSchema = external_exports.object({
  text: StringValueSchema.optional(),
  label: StringValueSchema.describe("A label, title, or placeholder text."),
  textFieldType: external_exports.enum(["shortText", "number", "date", "longText", "obscured"]).optional(),
  validationRegexp: external_exports.string().optional().describe("A regex string to validate the input.")
});
var DateTimeInputSchema = external_exports.object({
  value: StringValueSchema,
  enableDate: external_exports.boolean().optional(),
  enableTime: external_exports.boolean().optional(),
  outputFormat: external_exports.string().optional().describe("The string format for the output (e.g., 'YYYY-MM-DD').")
});
var MultipleChoiceSchema = external_exports.object({
  selections: external_exports.object({
    path: external_exports.string().describe("A data binding reference to a location in the data model (e.g., '/user/name').").optional(),
    literalArray: external_exports.array(external_exports.string()).optional()
  }).strict().superRefine(exactlyOneKey),
  options: external_exports.array(external_exports.object({
    label: external_exports.object({
      path: external_exports.string().describe("A data binding reference to a location in the data model (e.g., '/user/name').").optional(),
      literalString: external_exports.string().describe("A fixed, hardcoded string value.").optional()
    }).strict().superRefine(exactlyOneKey),
    value: external_exports.string()
  })).optional(),
  maxAllowedSelections: external_exports.number().optional(),
  type: external_exports.enum(["checkbox", "chips"]).optional(),
  filterable: external_exports.boolean().optional()
});
var SliderSchema = external_exports.object({
  value: external_exports.object({
    path: external_exports.string().describe("A data binding reference to a location in the data model (e.g., '/user/name').").optional(),
    literalNumber: external_exports.number().optional()
  }).strict().superRefine(exactlyOneKey),
  minValue: external_exports.number().optional(),
  maxValue: external_exports.number().optional(),
  label: StringValueSchema.optional()
});
var ComponentArrayTemplateSchema = external_exports.object({
  componentId: external_exports.string(),
  dataBinding: external_exports.string()
});
var ComponentArrayReferenceSchema = external_exports.object({
  explicitList: external_exports.array(external_exports.string()).optional(),
  template: ComponentArrayTemplateSchema.describe("A template for generating a dynamic list of children from a data model list. `componentId` is the component to use as a template, and `dataBinding` is the path to the map of components in the data model. Values in the map will define the list of children.").optional()
}).strict().superRefine(exactlyOneKey);
var RowSchema = external_exports.object({
  children: ComponentArrayReferenceSchema,
  distribution: external_exports.enum(["start", "center", "end", "spaceBetween", "spaceAround", "spaceEvenly"]).optional(),
  alignment: external_exports.enum(["start", "center", "end", "stretch"]).optional()
});
var ColumnSchema = external_exports.object({
  children: ComponentArrayReferenceSchema,
  distribution: external_exports.enum(["start", "center", "end", "spaceBetween", "spaceAround", "spaceEvenly"]).optional(),
  alignment: external_exports.enum(["start", "center", "end", "stretch"]).optional()
});
var ListSchema = external_exports.object({
  children: ComponentArrayReferenceSchema,
  direction: external_exports.enum(["vertical", "horizontal"]).optional(),
  alignment: external_exports.enum(["start", "center", "end", "stretch"]).optional()
});
var CardSchema = external_exports.object({
  child: external_exports.string().describe("The ID of the component to be rendered inside the card.")
});

// node_modules/@a2ui/web_core/src/v0_8/schema/server-to-client.js
var ValueMapSchema = DataValueSchema.describe("A single data entry. Exactly one 'value*' property should be provided alongside the key.");
var ComponentPropertiesSchema = external_exports.record(external_exports.any()).refine((val) => Object.keys(val).length === 1, "A wrapper object that MUST contain exactly one key, which is the name of the component type (e.g., 'Heading').");
var ComponentInstanceSchema = external_exports.object({
  id: external_exports.string().describe("The unique identifier for this component."),
  weight: external_exports.number().optional().describe("The relative weight of this component within a Row or Column. This corresponds to the CSS 'flex-grow' property. Note: this may ONLY be set when the component is a direct descendant of a Row or Column."),
  component: ComponentPropertiesSchema.describe("A wrapper object that MUST contain exactly one key, which is the name of the component type (e.g., 'Heading'). The value is an object containing the properties for that specific component.")
}).strict().describe("Represents a *single* component in a UI widget tree. This component could be one of many supported types.");
var BeginRenderingMessageSchema = external_exports.object({
  surfaceId: external_exports.string().describe("The unique identifier for the UI surface to be rendered."),
  catalogId: external_exports.string().optional().describe("The identifier of the component catalog to use for this surface. If omitted, the client MUST default to the standard catalog for this A2UI version (https://a2ui.org/specification/v0_8/standard_catalog_definition.json)."),
  root: external_exports.string().describe("The ID of the root component to render."),
  styles: external_exports.object({
    font: external_exports.string().optional().describe("The primary font for the UI."),
    primaryColor: external_exports.string().regex(/^#[0-9a-fA-F]{6}$/).optional().describe("The primary UI color as a hexadecimal code (e.g., '#00BFFF').")
  }).strict().optional().describe("Styling information for the UI.")
}).strict().describe("Signals the client to begin rendering a surface with a root component and specific styles.");
var SurfaceUpdateMessageSchema = external_exports.object({
  surfaceId: external_exports.string().describe("The unique identifier for the UI surface to be updated. If you are adding a new surface this *must* be a new, unique identified that has never been used for any existing surfaces shown."),
  components: external_exports.array(ComponentInstanceSchema).min(1).describe("A list containing all UI components for the surface.")
}).strict().superRefine((data, ctx) => {
  const componentIds = /* @__PURE__ */ new Set();
  for (const c5 of data.components) {
    if (c5.id) {
      if (componentIds.has(c5.id)) {
        ctx.addIssue({
          code: external_exports.ZodIssueCode.custom,
          message: `Duplicate component ID found: ${c5.id}`,
          path: ["components"]
        });
      }
      componentIds.add(c5.id);
    }
  }
  const checkRefs = (ids, componentId) => {
    for (const id of ids) {
      if (id && !componentIds.has(id)) {
        ctx.addIssue({
          code: external_exports.ZodIssueCode.custom,
          message: `Component '${componentId}' references non-existent component ID '${id}'.`,
          path: ["components"]
        });
      }
    }
  };
  for (const component of data.components) {
    if (!component.id || !component.component)
      continue;
    const componentTypes = Object.keys(component.component);
    if (componentTypes.length !== 1)
      continue;
    const componentType = componentTypes[0];
    const properties = component.component[componentType];
    switch (componentType) {
      case "Row":
      case "Column":
      case "List":
        if (properties.children && !Array.isArray(properties.children)) {
          const hasExplicit = !!properties.children.explicitList;
          const hasTemplate = !!properties.children.template;
          if (hasExplicit && hasTemplate || !hasExplicit && !hasTemplate) {
            ctx.addIssue({
              code: external_exports.ZodIssueCode.custom,
              message: `Component '${component.id}' must have either 'explicitList' or 'template' in children, but not both or neither.`
            });
          }
          if (hasExplicit)
            checkRefs(properties.children.explicitList, component.id);
          if (hasTemplate)
            checkRefs([properties.children.template?.componentId], component.id);
        }
        break;
      case "Card":
        if (properties.child)
          checkRefs([properties.child], component.id);
        break;
      case "Tabs":
        if (properties.tabItems && Array.isArray(properties.tabItems)) {
          properties.tabItems.forEach((tab) => {
            if (tab.child)
              checkRefs([tab.child], component.id);
          });
        }
        break;
      case "Modal":
        checkRefs([properties.entryPointChild, properties.contentChild], component.id);
        break;
      case "Button":
        if (properties.child)
          checkRefs([properties.child], component.id);
        break;
    }
  }
}).describe("Updates a surface with a new set of components.");
var DataModelUpdateMessageSchema = external_exports.object({
  surfaceId: external_exports.string().describe("The unique identifier for the UI surface this data model update applies to."),
  path: external_exports.string().optional().describe("An optional path to a location within the data model (e.g., '/user/name'). If omitted, or set to '/', the entire data model will be replaced."),
  contents: external_exports.array(ValueMapSchema).describe("An array of data entries. Each entry must contain a 'key' and exactly one corresponding typed 'value*' property.")
}).strict().describe("Updates the data model for a surface.");
var DeleteSurfaceMessageSchema = external_exports.object({
  surfaceId: external_exports.string().describe("The unique identifier for the UI surface to be deleted.")
}).strict().describe("Signals the client to delete the surface identified by 'surfaceId'.");
var A2uiMessageSchema = external_exports.object({
  beginRendering: BeginRenderingMessageSchema.optional(),
  surfaceUpdate: SurfaceUpdateMessageSchema.optional(),
  dataModelUpdate: DataModelUpdateMessageSchema.optional(),
  deleteSurface: DeleteSurfaceMessageSchema.optional()
}).strict().superRefine((data, ctx) => {
  const keys = Object.keys(data).filter((k2) => ["beginRendering", "surfaceUpdate", "dataModelUpdate", "deleteSurface"].includes(k2));
  if (keys.length !== 1) {
    ctx.addIssue({
      code: external_exports.ZodIssueCode.custom,
      message: "A2UI Protocol message must have exactly one of: surfaceUpdate, dataModelUpdate, beginRendering, deleteSurface."
    });
  }
}).describe("Describes a JSON payload for an A2UI (Agent to UI) message, which is used to dynamically construct and update user interfaces. A message MUST contain exactly ONE of the action properties: 'beginRendering', 'surfaceUpdate', 'dataModelUpdate', or 'deleteSurface'.");

// node_modules/@a2ui/web_core/src/processing/adapters/v0_8.js
function normalizeV08Component(comp) {
  if (!comp || typeof comp !== "object") {
    return { id: "", component: "" };
  }
  const c5 = comp;
  let componentName = typeof c5.component === "string" ? c5.component : "";
  const props = {};
  for (const [k2, v3] of Object.entries(c5)) {
    if (k2 !== "component" && k2 !== "id") {
      props[k2] = v3;
    }
  }
  if (c5.component && typeof c5.component === "object") {
    const obj = c5.component;
    const keys = Object.keys(obj);
    if (keys.length > 0) {
      componentName = keys[0];
      const compProps = obj[keys[0]];
      if (compProps && typeof compProps === "object" && compProps !== null && !Array.isArray(compProps)) {
        Object.assign(props, compProps);
      }
    }
  }
  return {
    ...props,
    id: String(c5.id ?? ""),
    component: componentName
  };
}
var V0Point8Adapter = class extends BaseVersionAdapter {
  constructor() {
    super(...arguments);
    this.version = "v0.8";
    this.schema = A2uiMessageSchema;
  }
  getNativeActionKeys() {
    return ["beginRendering", "surfaceUpdate", "dataModelUpdate", "deleteSurface"];
  }
  preparePayloadForValidation(msgObj) {
    const msgWithoutVersion = { ...msgObj };
    delete msgWithoutVersion.version;
    return msgWithoutVersion;
  }
  extractOperationsFromObject(msgObj) {
    const ops = [];
    if ("beginRendering" in msgObj) {
      const cs = msgObj.beginRendering;
      ops.push({
        type: "createSurface",
        surfaceId: String(cs?.surfaceId ?? ""),
        catalogId: typeof cs?.catalogId === "string" ? cs.catalogId : void 0,
        theme: cs?.theme ?? cs?.styles,
        sendDataModel: Boolean(cs?.sendDataModel),
        components: Array.isArray(cs?.components) ? cs.components.map(normalizeV08Component) : void 0,
        dataModel: cs?.dataModel && typeof cs.dataModel === "object" && !Array.isArray(cs.dataModel) ? cs.dataModel : void 0,
        version: typeof msgObj.version === "string" ? msgObj.version : this.version,
        rootId: typeof cs?.root === "string" && cs.root ? cs.root : void 0
      });
    }
    if ("surfaceUpdate" in msgObj) {
      const uc = msgObj.surfaceUpdate;
      ops.push({
        type: "updateComponents",
        surfaceId: String(uc?.surfaceId ?? ""),
        components: Array.isArray(uc?.components) ? uc.components.map(normalizeV08Component) : []
      });
    }
    if ("dataModelUpdate" in msgObj) {
      const ud = msgObj.dataModelUpdate;
      const surfaceId = String(ud?.surfaceId ?? "");
      const rawPath = typeof ud?.path === "string" ? ud.path.trim() : "";
      const basePath = rawPath && rawPath !== "/" ? (rawPath.startsWith("/") ? rawPath : `/${rawPath}`).replace(/\/+$/, "") : "";
      const extractContentValue = (entry) => {
        if ("valueNumber" in entry)
          return entry.valueNumber;
        if ("valueString" in entry)
          return entry.valueString;
        if ("valueBoolean" in entry)
          return entry.valueBoolean;
        if ("valueObject" in entry)
          return entry.valueObject;
        if ("valueArray" in entry)
          return entry.valueArray;
        if (Array.isArray(entry.valueMap)) {
          const nested = {};
          for (const sub of entry.valueMap) {
            if (sub && typeof sub === "object" && typeof sub.key === "string") {
              nested[sub.key] = extractContentValue(sub);
            }
          }
          return nested;
        }
        return entry.value;
      };
      if (Array.isArray(ud?.contents)) {
        for (const item of ud.contents) {
          if (item && typeof item === "object" && typeof item.key === "string") {
            const val = extractContentValue(item);
            const itemKey = item.key.replace(/^\/+/, "");
            const fullPath = itemKey === "." || itemKey === "" ? basePath || "/" : `${basePath}/${itemKey}`;
            ops.push({
              type: "updateDataModel",
              surfaceId,
              path: fullPath,
              value: val
            });
          }
        }
      } else {
        ops.push({
          type: "updateDataModel",
          surfaceId,
          path: typeof ud?.path === "string" ? ud.path : void 0,
          value: ud?.value
        });
      }
    }
    if ("deleteSurface" in msgObj) {
      const ds = msgObj.deleteSurface;
      ops.push({
        type: "deleteSurface",
        surfaceId: String(ds?.surfaceId ?? "")
      });
    }
    return ops;
  }
};

// node_modules/@a2ui/web_core/src/v0_9/schema/server-to-client.js
var CreateSurfaceMessageSchema = external_exports.object({
  "version": external_exports.enum(["v0.9", "v0.9.1"]),
  "createSurface": external_exports.object({
    "surfaceId": external_exports.string().describe("The unique identifier for the UI surface to be rendered."),
    "catalogId": external_exports.string().describe("A string that uniquely identifies this catalog. It is recommended to prefix this with an internet domain that you own, to avoid conflicts e.g. mycompany.com:somecatalog'."),
    "theme": external_exports.any().optional(),
    "sendDataModel": external_exports.boolean().describe("If true, the client will send the full data model of this surface in the metadata of every A2A message sent to the server that created the surface. Defaults to false.").optional()
  }).strict().describe("Signals the client to create a new surface and begin rendering it. It is an error to send 'createSurface' for a surfaceId that already exists without first deleting it. When this message is sent, the client will expect 'updateComponents' and/or 'updateDataModel' messages for the same surfaceId that define the component tree.")
}).strict();
var UpdateComponentsMessageSchema = external_exports.object({
  "version": external_exports.enum(["v0.9", "v0.9.1"]),
  "updateComponents": external_exports.object({
    "surfaceId": external_exports.string().describe("The unique identifier for the UI surface to be updated."),
    "components": external_exports.array(external_exports.record(external_exports.string(), external_exports.any())).min(1).describe("A list containing all UI components for the surface.")
  }).strict().describe("Updates a surface with a new set of components. This message can be sent multiple times to update the component tree of an existing surface. One of the components in one of the components lists MUST have an 'id' of 'root' to serve as the root of the component tree. A createSurface message MUST have been previously sent for the 'surfaceId' in this message; the surface's catalog is the one specified by that createSurface.")
}).strict();
var UpdateDataModelMessageSchema = external_exports.object({
  "version": external_exports.enum(["v0.9", "v0.9.1"]),
  "updateDataModel": external_exports.object({
    "surfaceId": external_exports.string().describe("The unique identifier for the UI surface this data model update applies to."),
    "path": external_exports.string().describe("An optional path to a location within the data model (e.g., '/user/name'). If omitted, or set to '/', refers to the entire data model.").optional(),
    "value": external_exports.any().describe("The data to be updated in the data model. If present, the value at 'path' is replaced (or created). If omitted, the key at 'path' is removed.").optional()
  }).strict().describe("Updates the data model for an existing surface. This message can be sent multiple times to update the data model. A createSurface message MUST have been previously sent for the 'surfaceId' in this message; the surface's catalog is the one specified by that createSurface.")
}).strict();
var DeleteSurfaceMessageSchema2 = external_exports.object({
  "version": external_exports.enum(["v0.9", "v0.9.1"]),
  "deleteSurface": external_exports.object({
    "surfaceId": external_exports.string().describe("The unique identifier for the UI surface to be deleted.")
  }).strict().describe("Signals the client to delete the surface identified by 'surfaceId'. A createSurface message MUST have been previously sent for the 'surfaceId' in this message; the surface's catalog is the one specified by that createSurface.")
}).strict();
var A2uiMessageSchema2 = external_exports.union([
  CreateSurfaceMessageSchema,
  UpdateComponentsMessageSchema,
  UpdateDataModelMessageSchema,
  DeleteSurfaceMessageSchema2
]);
var A2uiMessageListSchema = external_exports.array(A2uiMessageSchema2).describe("A list of messages.");
var A2uiMessageListWrapperSchema = external_exports.object({
  messages: A2uiMessageListSchema
}).strict().describe("An object wrapping a list of messages.");

// node_modules/@a2ui/web_core/src/processing/adapters/v0_9.js
var V0Point9Adapter = class extends BaseVersionAdapter {
  constructor() {
    super(...arguments);
    this.version = "v0.9";
    this.schema = A2uiMessageSchema2;
  }
  get compatibleCatalogVersions() {
    return /* @__PURE__ */ new Set(["0.9", "0.9.1"]);
  }
  getNativeActionKeys() {
    return ["createSurface", "updateComponents", "updateDataModel", "deleteSurface"];
  }
  extractOperationsFromObject(msgObj) {
    const ops = [];
    if ("createSurface" in msgObj) {
      const cs = msgObj.createSurface;
      ops.push({
        type: "createSurface",
        surfaceId: String(cs?.surfaceId || ""),
        catalogId: typeof cs?.catalogId === "string" ? cs.catalogId : void 0,
        theme: cs?.theme,
        sendDataModel: Boolean(cs?.sendDataModel),
        components: Array.isArray(cs?.components) ? cs.components : void 0,
        dataModel: cs?.dataModel && typeof cs.dataModel === "object" && !Array.isArray(cs.dataModel) ? cs.dataModel : void 0,
        version: typeof msgObj.version === "string" ? msgObj.version : this.version
      });
    }
    if ("updateComponents" in msgObj) {
      const uc = msgObj.updateComponents;
      ops.push({
        type: "updateComponents",
        surfaceId: String(uc?.surfaceId || ""),
        components: Array.isArray(uc?.components) ? uc.components : []
      });
    }
    if ("updateDataModel" in msgObj) {
      const ud = msgObj.updateDataModel;
      ops.push({
        type: "updateDataModel",
        surfaceId: String(ud?.surfaceId || ""),
        path: typeof ud?.path === "string" ? ud.path : void 0,
        value: ud?.value
      });
    }
    if ("deleteSurface" in msgObj) {
      const ds = msgObj.deleteSurface;
      ops.push({
        type: "deleteSurface",
        surfaceId: String(ds?.surfaceId || "")
      });
    }
    return ops;
  }
};

// node_modules/@a2ui/web_core/src/processing/operations.js
var INTERNAL_OPERATION_TYPES = [
  "createSurface",
  "updateComponents",
  "updateDataModel",
  "deleteSurface",
  "callRendererFunction",
  "agentFunctionResponse"
];
function isInternalOperation(payload) {
  return typeof payload === "object" && payload !== null && "type" in payload && typeof payload.type === "string" && INTERNAL_OPERATION_TYPES.includes(payload.type);
}

// node_modules/@a2ui/web_core/src/v1_0/schema/helpers.js
var TemplateChildListSchema = external_exports.object({
  "componentId": external_exports.lazy(() => ComponentIdSchema2),
  "path": external_exports.string().describe("The path to the list of component property objects in the data model.")
}).strict().describe("REF:#/$defs/TemplateChildList|A template for generating a dynamic list of children from a data model list. The `componentId` is the component to use as a template.");
var AnyComponentSchema2 = external_exports.lazy(() => ComponentCommonSchema2.extend({
  component: external_exports.string()
}).passthrough().refine((comp) => comp.component !== "Surface", {
  message: 'Component type cannot be "Surface". "Surface" is a top-level protocol container defined in createSurface, not a child component.'
}));
var ComponentsListSchema = external_exports.array(AnyComponentSchema2).min(1);
var RendererCapabilitiesSchema = external_exports.record(external_exports.string(), external_exports.any());

// node_modules/@a2ui/web_core/src/v1_0/schema/agent-to-renderer.js
var CreateSurfaceMessageSchema2 = external_exports.object({
  "version": external_exports.literal("1.0"),
  "createSurface": external_exports.object({
    "surfaceId": external_exports.string().describe("The unique identifier for the UI surface to be rendered. It must be globally unique for the renderer's lifetime."),
    "catalogId": external_exports.string().describe("A string that uniquely identifies the default catalog for this surface. It is recommended to prefix this with an internet domain that you own, to avoid conflicts e.g. 'mycompany.com:somecatalog'. Components and function calls that do not explicitly specify a catalogId will use this surface-level default catalogId.").optional(),
    "sendDataModel": external_exports.boolean().describe("If true, the renderer will send the full data model of this surface in the metadata of every A2A message sent to the agent that created the surface. Defaults to false.").optional(),
    "components": ComponentsListSchema.optional(),
    "dataModel": external_exports.record(external_exports.string(), external_exports.any()).describe("The initial root data model object for the surface.").optional(),
    "metadata": external_exports.object({ "extensions": ExtensionsSchema2.optional() }).strict().describe("Optional surface-level metadata.").optional()
  }).strict().describe("Signals the renderer to create a new surface and begin rendering it. Creating a surface implicitly instantiates the canonical 'Surface' container component ('common_types.json#/$defs/Surface') with 'child': 'root'. It is an error to try to create a surface with an existing ID without first deleting it; surfaceId MUST be globally unique for the renderer's lifetime. The component tree and initial data model may be provided inline via 'components' and 'dataModel', or sent later in 'updateComponents' and/or 'updateDataModel' messages for the same surfaceId.")
}).strict();
var UpdateComponentsMessageSchema2 = external_exports.object({
  "version": external_exports.literal("1.0"),
  "updateComponents": external_exports.object({
    "surfaceId": external_exports.string().describe("The unique identifier for the UI surface to be updated. It must be globally unique for the renderer's lifetime."),
    "components": ComponentsListSchema
  }).strict().describe("Updates a surface with a new set of components. This message can be sent multiple times to update the component tree of an existing surface. One of the components in one of the components lists MUST have an 'id' of 'root' to serve as the root of the component tree. The createSurface message MUST have been previously sent for this surfaceId.")
}).strict();
var UpdateDataModelMessageSchema2 = external_exports.object({
  "version": external_exports.literal("1.0"),
  "updateDataModel": external_exports.object({
    "surfaceId": external_exports.string().describe("The unique identifier for the UI surface this data model update applies to. It must be globally unique for the renderer's lifetime."),
    "path": external_exports.string().describe("An optional path to a location within the data model (e.g., '/user/name'). If omitted, or set to '/', refers to the entire data model.").optional(),
    "value": external_exports.any().describe("The data to be updated in the data model. To delete the key/value at 'path', set 'value' explicitly to null.")
  }).strict().describe("Updates the data model for an existing surface. This message can be sent multiple times to update the data model. The createSurface message MUST have been previously sent for this surfaceId.")
}).strict();
var DeleteSurfaceMessageSchema3 = external_exports.object({
  "version": external_exports.literal("1.0"),
  "deleteSurface": external_exports.object({
    "surfaceId": external_exports.string().describe("The unique identifier for the UI surface to be deleted. It must be globally unique for the renderer's lifetime.")
  }).strict().describe("Signals the renderer to delete the surface identified by 'surfaceId'. The createSurface message MUST have been previously sent for this surfaceId.")
}).strict();
var CallRendererFunctionMessageSchema = external_exports.object({
  "version": external_exports.literal("1.0"),
  "callRendererFunction": external_exports.object({
    "surfaceId": external_exports.string().describe("Optional ID of the surface whose data model provides the evaluation context for this call. If omitted, the function executes in a surface-independent root context with an empty data model.").optional(),
    "functionCallId": CallIdSchema2,
    "callFunction": external_exports.intersection(FunctionCallSchema2, external_exports.object({ "catalogId": external_exports.string() }).passthrough())
  }).strict().describe("Signals the renderer to execute a function locally on behalf of the agent.")
}).strict();
var AgentFunctionResponseMessageSchema = external_exports.object({ "version": external_exports.literal("1.0"), "agentFunctionResponse": FunctionResponseSchema2 }).strict();
var AgentToRendererMessageSchema = external_exports.union([
  CreateSurfaceMessageSchema2,
  UpdateComponentsMessageSchema2,
  UpdateDataModelMessageSchema2,
  DeleteSurfaceMessageSchema3,
  CallRendererFunctionMessageSchema,
  AgentFunctionResponseMessageSchema
]);

// node_modules/@a2ui/web_core/src/processing/adapters/v1_0.js
function assertObjectArgs(cf, version, functionCallId) {
  if (Object.hasOwn(cf, "args") && cf.args !== void 0 && (typeof cf.args !== "object" || cf.args === null || Array.isArray(cf.args))) {
    throw new A2uiValidationError(`Invalid ${version} message: callRendererFunction.callFunction.args: Expected object, received ${Array.isArray(cf.args) ? "array" : typeof cf.args}`, void 0, "VALIDATION_ERROR", { functionCallId: functionCallId || void 0 });
  }
}
function extractCallRendererFunctionOp(crf, version) {
  const cf = crf?.callFunction || {};
  const functionCallId = String(crf?.functionCallId || "");
  assertObjectArgs(cf, version, functionCallId);
  return {
    type: "callRendererFunction",
    surfaceId: typeof crf?.surfaceId === "string" ? crf.surfaceId : void 0,
    functionCallId,
    call: String(cf?.["@call"] || cf?.call || ""),
    version,
    catalogId: typeof cf?.catalogId === "string" ? cf?.catalogId : void 0,
    args: cf?.args && typeof cf.args === "object" && !Array.isArray(cf.args) ? cf.args : {}
  };
}
var V1Point0Adapter = class extends BaseVersionAdapter {
  constructor() {
    super(...arguments);
    this.version = "1.0";
    this.schema = AgentToRendererMessageSchema;
  }
  getNativeActionKeys() {
    return [...INTERNAL_OPERATION_TYPES];
  }
  extractOperationsFromObject(msgObj) {
    const ops = [];
    if ("createSurface" in msgObj) {
      const cs = msgObj.createSurface;
      ops.push({
        type: "createSurface",
        surfaceId: String(cs?.surfaceId || ""),
        catalogId: typeof cs?.catalogId === "string" ? cs.catalogId : void 0,
        theme: cs?.theme,
        sendDataModel: Boolean(cs?.sendDataModel),
        components: Array.isArray(cs?.components) ? cs.components : void 0,
        dataModel: cs?.dataModel && typeof cs.dataModel === "object" && !Array.isArray(cs.dataModel) ? cs.dataModel : void 0,
        version: typeof msgObj.version === "string" ? msgObj.version : this.version,
        metadata: cs?.metadata && typeof cs.metadata === "object" && !Array.isArray(cs.metadata) ? cs.metadata : void 0
      });
    }
    if ("updateComponents" in msgObj) {
      const uc = msgObj.updateComponents;
      ops.push({
        type: "updateComponents",
        surfaceId: String(uc?.surfaceId || ""),
        components: Array.isArray(uc?.components) ? uc.components : []
      });
    }
    if ("updateDataModel" in msgObj) {
      const ud = msgObj.updateDataModel;
      ops.push({
        type: "updateDataModel",
        surfaceId: String(ud?.surfaceId || ""),
        path: typeof ud?.path === "string" ? ud.path : void 0,
        value: ud?.value
      });
    }
    if ("deleteSurface" in msgObj) {
      const ds = msgObj.deleteSurface;
      ops.push({
        type: "deleteSurface",
        surfaceId: String(ds?.surfaceId || "")
      });
    }
    if ("callRendererFunction" in msgObj) {
      ops.push(extractCallRendererFunctionOp(msgObj.callRendererFunction, this.version));
    }
    if ("agentFunctionResponse" in msgObj) {
      const afr = msgObj.agentFunctionResponse;
      ops.push({
        type: "agentFunctionResponse",
        functionCallId: String(afr?.functionCallId || ""),
        version: this.version,
        value: afr?.value,
        error: afr?.error && typeof afr.error === "object" ? afr.error : void 0
      });
    }
    return ops;
  }
};

// node_modules/@a2ui/web_core/src/processing/adapters/factory.js
var VersionAdapterFactory = class {
  constructor() {
    this.adapters = /* @__PURE__ */ new Map([
      ["0.8", new V0Point8Adapter()],
      ["0.9", new V0Point9Adapter()],
      ["0.9.1", new V0Point9Adapter()],
      ["1.0", new V1Point0Adapter()]
    ]);
  }
  /**
   * Returns the aggregated set of all action keys supported by all registered adapters.
   *
   * @return A set of all supported action key strings.
   */
  getAllKnownActions() {
    const actions = /* @__PURE__ */ new Set();
    for (const adapter of this.adapters.values()) {
      if (adapter.validActions) {
        for (const action of adapter.validActions) {
          actions.add(action);
        }
      }
    }
    return actions;
  }
  /**
   * Dynamically registers a version adapter on this factory instance.
   *
   * @param adapter The version adapter instance to register.
   */
  registerAdapter(adapter) {
    const key = toCanonicalVersion(adapter.version) ?? normalizeVersionString(adapter.version);
    this.adapters.set(key, adapter);
  }
  /**
   * Resolves the version adapter for the specified version string from this factory instance.
   *
   * @param version The protocol version string (e.g. '1.0').
   * @return The matching version adapter.
   * @throws A2uiValidationError if the version string is unsupported.
   */
  getAdapter(version) {
    const key = toCanonicalVersion(version) ?? normalizeVersionString(version);
    const adapter = this.adapters.get(key);
    if (!adapter) {
      const supported = Array.from(new Set(Array.from(this.adapters.entries()).map(([k2, a4]) => k2 === "0.9.1" ? "v0.9.1" : a4.version))).sort().join(", ");
      throw new A2uiValidationError(`[VersionAdapterFactory] Unsupported protocol version '${version}'. Supported versions: ${supported}.`);
    }
    return adapter;
  }
  /**
   * Resolves the version adapter directly from an incoming message payload using this factory instance.
   *
   * @param payload The raw JSON message payload.
   * @returns The resolved version adapter.
   * @throws A2uiValidationError if the payload is missing a valid 'version' string.
   */
  resolveFromPayload(payload) {
    const item = Array.isArray(payload) ? payload[0] : payload;
    if (typeof item === "object" && item !== null) {
      if ("messages" in item && Array.isArray(item.messages)) {
        return this.resolveFromPayload(item.messages);
      }
      if ("version" in item) {
        const ver = item.version;
        if (typeof ver !== "string") {
          throw new A2uiValidationError(`[VersionAdapterFactory] Message payload is missing a valid 'version' string: 'version' property must be a string, got ${typeof ver}.`);
        }
        return this.getAdapter(ver);
      }
      if ("beginRendering" in item || "surfaceUpdate" in item || "dataModelUpdate" in item || "deleteSurface" in item) {
        return this.getAdapter("v0.8");
      }
    }
    throw new A2uiValidationError("[VersionAdapterFactory] Message payload is missing a valid 'version' string.");
  }
  /**
   * Registers a version adapter on the default singleton factory instance.
   *
   * @param adapter Version adapter instance to register.
   */
  static registerAdapter(adapter) {
    defaultVersionAdapterFactory.registerAdapter(adapter);
  }
  /**
   * Resolves a version adapter for the specified version from the default singleton factory instance.
   *
   * @param version Protocol version string.
   * @return Matching version adapter.
   */
  static getAdapter(version) {
    return defaultVersionAdapterFactory.getAdapter(version);
  }
  /**
   * Resolves a version adapter directly from a payload using the default singleton factory instance.
   *
   * @param payload Raw JSON message payload.
   * @returns Resolved version adapter.
   */
  static resolveFromPayload(payload) {
    return defaultVersionAdapterFactory.resolveFromPayload(payload);
  }
  /**
   * Returns the aggregated set of all action keys supported by all registered adapters
   * from the default singleton factory instance.
   *
   * @return A set of all supported action key strings.
   */
  static getAllKnownActions() {
    return defaultVersionAdapterFactory.getAllKnownActions();
  }
};
var defaultVersionAdapterFactory = new VersionAdapterFactory();
registerKnownActionsProvider(() => defaultVersionAdapterFactory.getAllKnownActions());

// node_modules/@a2ui/web_core/src/validation/payload-validator.js
var COMPONENT_ENVELOPE_KEYS2 = ["id", "component", "catalogId", "metadata"];
function getSystemFunctionApi(name) {
  return name === IndexApi.name ? IndexApi : void 0;
}
function assertSystemCallNamesNoCatalog(name, call) {
  if (call["catalogId"] !== void 0) {
    throw new A2uiValidationError(`System function '${name}' belongs to no catalog and must not name a catalogId.`);
  }
}
function nonStringCatalogIdMessage(name) {
  return `Function call '${name}' has a non-string 'catalogId'.`;
}
function nestedCallRunsInCatalog(callCatalogId, catalogId, { catalogIsDefault }) {
  if (callCatalogId === void 0) {
    return catalogIsDefault;
  }
  return callCatalogId === catalogId;
}
var PayloadValidator = class {
  /**
   * Initializes a new `PayloadValidator`.
   *
   * @param catalog Catalog whose schemas define what a valid payload is.
   * @param config Validation strictness. When omitted, unknown components and
   *   functions are tolerated.
   */
  constructor(catalog, config) {
    this.catalog = catalog;
    this.config = config;
    this.enforceIdentifiers = isAtLeastVersion(catalog.protocolVersion, "1.0");
  }
  /**
   * Whether unrecognized components and functions are tolerated.
   *
   * An absent config means the caller is not validating at all, so unknown
   * elements pass. This preserves the gate `MessageProcessor` applied before
   * this class existed, and matches Python, whose `validate_components_update`
   * returns early when `config is None`.
   */
  get allowUnknown() {
    return this.config ? this.config.allowUnknownElements ?? false : true;
  }
  /**
   * Validates a single component payload against its catalog definition,
   * including any nested function calls within its properties.
   *
   * @param comp Raw component payload, including its `id` and `component`
   *   envelope keys.
   * @param knownType Component type to assume when the payload omits
   *   `component`, as happens on a partial update to an existing component.
   * @param options Validation options. `skipNestedFunctions` leaves the
   *   function calls in the properties unchecked, as pre-1.0 renderers did
   *   when validation was off.
   * @throws {A2uiValidationError} If the identifier, the component type, any
   *   property, or any nested function call fails validation.
   */
  validateComponent(comp, knownType, options) {
    const id = comp["id"];
    if (typeof id !== "string" || !id) {
      const msg = `Component '${comp["component"]}' is missing an 'id'; entries require a valid string 'id'.`;
      throw new A2uiValidationError(msg, [{ path: "/id", code: "missing_field", message: msg }], "VALIDATION_ERROR", { path: "/id" });
    }
    this.assertIdentifier(id, `Component id '${id}' must be a valid UAX #31 identifier`, false, "/id");
    const declaredType = comp["component"];
    const componentType = typeof declaredType === "string" ? declaredType : knownType;
    if (!componentType) {
      const msg = `Cannot create component ${id} without a type.`;
      throw new A2uiValidationError(msg, [{ path: "/component", code: "missing_field", message: msg }], "VALIDATION_ERROR", { path: "/component" });
    }
    const rawCatalogId = comp["catalogId"];
    const componentCatalogId = typeof rawCatalogId === "string" ? rawCatalogId : void 0;
    const properties = stripEnvelopeKeys(comp);
    const componentApi = this.catalog.components.get(componentType);
    if (!componentApi) {
      if (!this.allowUnknown) {
        const msg = `Unknown component type '${componentType}' not found in catalog '${this.catalog.id}'.`;
        throw new A2uiValidationError(msg, [{ path: "/component", code: "invalid_value", message: msg }], "VALIDATION_ERROR", { path: "/component" });
      }
      if (!options?.skipNestedFunctions) {
        this.validateNestedFunctions(properties, componentCatalogId);
      }
      return;
    }
    const knownKeys = getKnownSchemaKeys(componentApi.schema);
    this.validateCommonEnvelopeFields(properties, knownKeys, componentType, id, componentCatalogId);
    const result = componentApi.schema.safeParse(properties);
    if (!result.success) {
      const formattedErrors = result.error.errors.map(formatZodIssue).join(", ");
      const details = zodIssuesToDetails(result.error.issues);
      const uniquePaths = new Set(details.map((d4) => d4.path));
      const firstPath = uniquePaths.size === 1 ? details[0]?.path : void 0;
      throw new A2uiValidationError(`Validation failed for component '${componentType}' (${id}): ${formattedErrors}`, details, "VALIDATION_ERROR", { path: firstPath && firstPath !== "/" ? firstPath : void 0 });
    }
    if (!options?.skipNestedFunctions) {
      this.validateNestedFunctions(properties, componentCatalogId);
    }
  }
  /**
   * Validates `accessibility` and `metadata` from `ComponentCommon` when the
   * component schema does not explicitly declare them, removing them from
   * `properties` before strict component schema parsing.
   */
  validateCommonEnvelopeFields(properties, knownKeys, componentType, id, componentCatalogId) {
    if ("accessibility" in properties && !knownKeys?.has("accessibility")) {
      const accVal = properties["accessibility"];
      delete properties["accessibility"];
      const accResult = AccessibilityAttributesSchema.optional().safeParse(accVal);
      if (!accResult.success) {
        const formattedErrors = accResult.error.errors.map(formatZodIssue).join(", ");
        const details = zodIssuesToDetails(accResult.error.issues, "/accessibility");
        throw new A2uiValidationError(`Validation failed for component '${componentType}' (${id}): ${formattedErrors}`, details, "VALIDATION_ERROR", { path: details[0]?.path });
      }
      this.validateNestedFunctions(accVal, componentCatalogId, "/accessibility");
    }
    if ("metadata" in properties && !knownKeys?.has("metadata")) {
      const metaVal = properties["metadata"];
      delete properties["metadata"];
      const metaResult = ComponentCommonSchema.shape.metadata.safeParse(metaVal);
      if (!metaResult.success) {
        const formattedErrors = metaResult.error.errors.map(formatZodIssue).join(", ");
        const details = zodIssuesToDetails(metaResult.error.issues, "/metadata");
        throw new A2uiValidationError(`Validation failed for component '${componentType}' (${id}): ${formattedErrors}`, details, "VALIDATION_ERROR", { path: details[0]?.path });
      }
    }
  }
  /**
   * Asserts that a v1.0 call's envelope is well formed without consulting its
   * catalog: the function name and all argument keys satisfy UAX #31
   * identifier syntax, and `args`, when present, is an object.
   *
   * @param name Function name to check.
   * @param args Function arguments object whose keys are checked.
   * @throws {A2uiValidationError} If the function name or any argument key is
   *   not a valid UAX #31 identifier, or `args` is not an object.
   */
  assertFunctionIdentifiers(name, args) {
    this.assertIdentifier(name, `Function name '${name}' must be a valid UAX #31 identifier`, true);
    if (this.enforceIdentifiers && args !== void 0 && args !== null && (typeof args !== "object" || Array.isArray(args))) {
      throw new A2uiValidationError(`Validation failed for function '${name}': Expected object, received ${Array.isArray(args) ? "array" : typeof args}`);
    }
    if (args && typeof args === "object" && !Array.isArray(args)) {
      const keys = Object.keys(args);
      if (keys.length > MAX_FUNCTION_CALL_ARGS) {
        throw new A2uiValidationError(`Function call '${name}' exceeds maximum allowed arguments count (${MAX_FUNCTION_CALL_ARGS})`);
      }
      for (const argName of keys) {
        this.assertIdentifier(argName, `Function argument '${argName}' in function '${name}' must be a valid UAX #31 identifier`);
      }
    }
  }
  /**
   * Recursively walks a component's property values and validates any nested
   * function call objects.
   *
   * From v1.0 a call that runs in this catalog (see `nestedCallRunsInCatalog`)
   * is validated against this catalog. Any other call runs in a catalog this
   * validator cannot see, so only its envelope (name and argument
   * identifiers) is checked; `MessageProcessor` validates it against the
   * catalog it resolves to. A non-string `catalogId` is rejected. Reserved `@`
   * system functions belong to no catalog and are validated fully.
   *
   * @param val Property value or subtree to inspect.
   * @param componentCatalogId The `catalogId` the component names, if any.
   * @throws {A2uiValidationError} If any nested function call fails schema or identifier validation.
   */
  checkNestedCallRecord(record, rawName, componentCatalogId) {
    const rawArgs = record["args"];
    const argsDict = rawArgs !== void 0 ? rawArgs : {};
    if (!this.enforceIdentifiers) {
      this.validateFunction(rawName, argsDict);
      return;
    }
    if (rawName.startsWith("@")) {
      assertSystemCallNamesNoCatalog(rawName, record);
      this.validateFunction(rawName, argsDict);
      return;
    }
    const callCatalogId = record["catalogId"];
    if (callCatalogId !== void 0 && typeof callCatalogId !== "string") {
      throw new A2uiValidationError(nonStringCatalogIdMessage(rawName));
    }
    if (nestedCallRunsInCatalog(callCatalogId, this.catalog.id, {
      catalogIsDefault: componentCatalogId === void 0
    })) {
      this.validateFunction(rawName, argsDict);
    } else {
      this.assertFunctionIdentifiers(rawName, argsDict);
    }
  }
  validateNestedFunctions(val, componentCatalogId, currentPath = "") {
    if (Array.isArray(val)) {
      for (let i7 = 0; i7 < val.length; i7++) {
        this.validateNestedFunctions(val[i7], componentCatalogId, `${currentPath}/${i7}`);
      }
      return;
    }
    if (typeof val !== "object" || val === null) {
      return;
    }
    const record = val;
    if (this.enforceIdentifiers) {
      validateReservedDirectives(Object.keys(record), this.catalog.protocolVersion);
    }
    const rawName = this.enforceIdentifiers ? record["@call"] : record["call"] ?? record["function"];
    if (typeof rawName === "string" && (this.enforceIdentifiers || rawName.length > 0)) {
      try {
        this.checkNestedCallRecord(record, rawName, componentCatalogId);
      } catch (err) {
        if (err instanceof A2uiValidationError && currentPath) {
          err.path = err.path && err.path !== "/" ? `${currentPath}${err.path}` : currentPath;
        }
        throw err;
      }
    }
    for (const [k2, v3] of Object.entries(record)) {
      this.validateNestedFunctions(v3, componentCatalogId, `${currentPath}/${k2}`);
    }
  }
  /**
   * Validates the arguments of a function call against the catalog definition.
   *
   * @param name Name of the function being called.
   * @param args Arguments supplied by the payload.
   * @returns The parsed arguments object (including any schema defaults).
   * @throws {A2uiValidationError} If the function name, an argument name, or an
   *   argument value fails validation.
   */
  validateFunction(name, args) {
    this.assertFunctionIdentifiers(name, args);
    const fn = this.enforceIdentifiers && name.startsWith("@") ? getSystemFunctionApi(name) : this.catalog.functions.get(name);
    if (!fn) {
      if (!this.allowUnknown) {
        throw new A2uiValidationError(`Unrecognized function '${name}'`);
      }
      return args ?? {};
    }
    if (!fn.schema) {
      if (args !== void 0 && (typeof args !== "object" || args === null || Array.isArray(args))) {
        throw new A2uiValidationError(`Validation failed for function '${name}': Expected object, received ${Array.isArray(args) ? "array" : typeof args}`, void 0, "VALIDATION_ERROR", { path: "/args" });
      }
      return args ?? {};
    }
    const result = fn.schema.safeParse(args ?? {});
    if (!result.success) {
      const formattedErrors = result.error.errors.map(formatZodIssue).join(", ");
      const details = zodIssuesToDetails(result.error.issues, "/args");
      throw new A2uiValidationError(`Validation failed for function '${name}': ${formattedErrors}`, details, "VALIDATION_ERROR", { path: details[0]?.path });
    }
    return result.data;
  }
  /**
   * Validates a surface theme against the catalog's theme schema.
   *
   * A catalog that declares no theme schema accepts any theme.
   *
   * @param theme Theme payload supplied on `createSurface`.
   * @returns The parsed theme, which a schema may have defaulted or coerced.
   * @throws {A2uiValidationError} If the theme does not satisfy the schema.
   */
  validateTheme(theme) {
    const themeSchema = this.catalog.themeSchema;
    if (!themeSchema)
      return theme;
    const result = themeSchema.safeParse(theme);
    if (!result.success) {
      const formattedErrors = result.error.errors.map(formatZodIssue).join(", ");
      const details = zodIssuesToDetails(result.error.issues, "/theme");
      throw new A2uiValidationError(`Validation failed for theme: ${formattedErrors}`, details, "VALIDATION_ERROR", { path: details[0]?.path ?? "/theme" });
    }
    return result.data;
  }
  /**
   * Asserts that an identifier is valid if identifier enforcement is enabled.
   *
   * @param name Identifier to check.
   * @param message Error message to use when validation fails.
   * @param allowLeadingAt Whether a leading `@` is permitted.
   * @param path Optional JSON Pointer path for error context.
   * @throws {A2uiValidationError} If identifier enforcement is active and `name` is invalid.
   */
  assertIdentifier(name, message, allowLeadingAt = false, path) {
    if (!this.enforceIdentifiers)
      return;
    if (!isValidUax31Identifier(name, allowLeadingAt)) {
      throw new A2uiValidationError(message, path ? [{ path, code: "invalid_value", message }] : void 0, "VALIDATION_ERROR", { path });
    }
  }
};
function zodIssuesToDetails(issues, prefix = "") {
  return issues.map((issue) => {
    const segments = issue.path.map((s5) => String(s5).replace(/~/g, "~0").replace(/\//g, "~1"));
    if (issue.code === "unrecognized_keys" && "keys" in issue && Array.isArray(issue.keys) && issue.keys.length > 0) {
      segments.push(String(issue.keys[0]).replace(/~/g, "~0").replace(/\//g, "~1"));
    }
    const relPath = segments.length > 0 ? `/${segments.join("/")}` : "";
    const fullPath = `${prefix}${relPath}` || "/";
    const isMissing = issue.code === "invalid_type" && "received" in issue && issue.received === "undefined" || issue.message === "Required";
    return {
      path: fullPath,
      code: isMissing ? "missing_field" : "invalid_value",
      message: issue.message
    };
  });
}
function stripEnvelopeKeys(comp) {
  const properties = {};
  for (const [key, value] of Object.entries(comp)) {
    if (COMPONENT_ENVELOPE_KEYS2.includes(key))
      continue;
    properties[key] = value;
  }
  return properties;
}

// node_modules/@a2ui/web_core/src/rpc/rpc-handler.js
var RpcHandler = class {
  constructor(optionsOrCatalogs, outboundListener) {
    this.pendingAgentCalls = /* @__PURE__ */ new Map();
    this.isDisposed = false;
    if (Array.isArray(optionsOrCatalogs)) {
      this.catalogs = optionsOrCatalogs;
      this.outboundListener = outboundListener;
      this.defaultTimeoutMs = 3e4;
    } else {
      const options = optionsOrCatalogs ?? {};
      this.catalogs = options.catalogs ?? [];
      this.outboundListener = options.outboundListener;
      this.defaultTimeoutMs = options.defaultTimeoutMs ?? 3e4;
    }
  }
  /**
   * Whether this RPC handler has been disposed.
   */
  get disposed() {
    return this.isDisposed;
  }
  /**
   * Executes a remote renderer function requested by the server agent.
   *
   * @param message Inbound callRendererFunction message.
   * @param context DataContext for function execution.
   * @param isUserActivated Whether execution occurs in an active user gesture context.
   * @returns A promise resolving to the renderer function response message.
   */
  async handleCallRendererFunction(message, context, isUserActivated = false) {
    const validationError = this.validateInboundMessage(message);
    if (validationError) {
      await this.emitOutboundResponse(validationError);
      return validationError;
    }
    const { version, callRendererFunction } = message;
    const { surfaceId, functionCallId, callFunction } = callRendererFunction;
    if (surfaceId !== void 0 && context?.surface?.id !== surfaceId) {
      const errorResponse = this.createResponseError(functionCallId, RpcErrorCode.INVALID_FUNCTION_CALL, `Surface not found: ${surfaceId}`, version);
      await this.emitOutboundResponse(errorResponse);
      return errorResponse;
    }
    const call = callFunction["@call"] ?? callFunction.call;
    const { catalogId, args } = callFunction;
    const resolved = this.resolveFunctionImplementation(catalogId, call, context, version);
    if ("error" in resolved) {
      const errorResponse = this.createResponseError(functionCallId, RpcErrorCode.INVALID_FUNCTION_CALL, resolved.error, version);
      await this.emitOutboundResponse(errorResponse);
      return errorResponse;
    }
    const accessError = this.checkExecutionPermissions(resolved.funcImpl, call, isUserActivated);
    if (accessError) {
      const errorResponse = this.createResponseError(functionCallId, RpcErrorCode.INVALID_FUNCTION_CALL, accessError, version);
      await this.emitOutboundResponse(errorResponse);
      return errorResponse;
    }
    const effectiveContext = this.ensureDataContext(context, resolved.catalog, functionCallId);
    const resolvedArgsResult = this.resolveDynamicArguments(args, effectiveContext, isUserActivated);
    if ("error" in resolvedArgsResult) {
      const errorResponse = this.createResponseError(functionCallId, RpcErrorCode.EXECUTION_ERROR, resolvedArgsResult.error, version);
      await this.emitOutboundResponse(errorResponse);
      return errorResponse;
    }
    let settledArgs;
    try {
      settledArgs = await settleDeep(resolvedArgsResult.args);
    } catch (err) {
      const errorResponse = this.createResponseError(functionCallId, RpcErrorCode.EXECUTION_ERROR, this.extractErrorMessage(err) || "An error occurred during function execution.", version);
      await this.emitOutboundResponse(errorResponse);
      return errorResponse;
    }
    const parsedArgsResult = this.parseArguments(resolved.catalog, resolved.funcImpl, settledArgs, call);
    if ("error" in parsedArgsResult) {
      const errorResponse = this.createResponseError(functionCallId, RpcErrorCode.INVALID_FUNCTION_CALL, parsedArgsResult.error, version);
      await this.emitOutboundResponse(errorResponse);
      return errorResponse;
    }
    const response = await this.executeFunctionSafely(resolved.funcImpl, parsedArgsResult.args, effectiveContext, functionCallId, version);
    await this.emitOutboundResponse(response);
    return response;
  }
  /**
   * Processes an inbound agent function response to resolve or reject the matching pending outbound call.
   *
   * @param message Inbound agent function response message.
   */
  handleAgentFunctionResponse(message) {
    if (!message || !message.agentFunctionResponse)
      return;
    const { functionCallId, value, error } = message.agentFunctionResponse;
    const pending = this.pendingAgentCalls.get(functionCallId);
    if (!pending)
      return;
    this.pendingAgentCalls.delete(functionCallId);
    if (error) {
      const errCode = error.code ?? RpcErrorCode.UNKNOWN_ERROR;
      const errMsg = error.message ?? "Agent function execution failed";
      pending.reject(new A2uiRpcError(errMsg, errCode, functionCallId, error));
    } else {
      pending.resolve(value);
    }
  }
  /**
   * Invokes a remote function on the server agent using an options bag.
   *
   * @template T Expected return value type of the agent function.
   * @param surfaceId Identifier of the surface requesting execution.
   * @param call Function call details.
   * @param options Optional invocation options (custom functionCallId, timeoutMs).
   * @returns A promise resolving to the agent function return value.
   * @throws {A2uiRpcError} If the handler is disposed, no listener is configured, the call is invalid or duplicate, or invocation times out or fails.
   */
  callAgentFunction(surfaceId, call, options) {
    if (this.isDisposed) {
      return Promise.reject(new A2uiRpcError("RpcHandler has been disposed.", RpcErrorCode.DISPOSED));
    }
    if (!this.outboundListener) {
      return Promise.reject(new A2uiRpcError("Cannot call agent function without outboundListener configured.", RpcErrorCode.NO_LISTENER));
    }
    const callName = call?.["@call"] ?? call?.call;
    if (!call || !callName) {
      return Promise.reject(new A2uiRpcError("Missing or invalid function call name.", RpcErrorCode.INVALID_FUNCTION_CALL));
    }
    const opts = options ?? {};
    const functionCallId = opts.functionCallId ?? this.generateFunctionCallId();
    const effectiveTimeoutMs = opts.timeoutMs ?? this.defaultTimeoutMs;
    if (this.pendingAgentCalls.has(functionCallId)) {
      return Promise.reject(new A2uiRpcError(`A call with functionCallId '${functionCallId}' is already pending.`, RpcErrorCode.DUPLICATE, functionCallId));
    }
    const normalized = {
      functionCallId,
      call,
      effectiveTimeoutMs,
      version: opts.version
    };
    return this.dispatchAgentCall(surfaceId, normalized);
  }
  /**
   * Disposes the RPC handler and cancels all pending agent function calls with a `CANCELLED` error.
   */
  dispose() {
    if (this.isDisposed)
      return;
    this.isDisposed = true;
    for (const [id, pending] of this.pendingAgentCalls.entries()) {
      pending.reject(new A2uiRpcError(`RpcHandler disposed while call '${id}' was pending.`, RpcErrorCode.CANCELLED, id));
    }
    this.pendingAgentCalls.clear();
  }
  /**
   * Sends an outbound renderer response message to the outbound listener.
   *
   * @param response Renderer function response message to transmit.
   */
  async emitOutboundResponse(response) {
    if (this.outboundListener) {
      try {
        const res = this.outboundListener(response);
        if (res && typeof res.catch === "function") {
          await res;
        }
      } catch {
      }
    }
  }
  /**
   * Validates an inbound renderer function call message.
   *
   * @param message Inbound message to validate.
   * @returns An error response if validation fails; otherwise `null`.
   */
  validateInboundMessage(message) {
    if (!message) {
      return this.createResponseError("unknown", RpcErrorCode.INVALID_FUNCTION_CALL, "Inbound message is null or undefined.", "unknown");
    }
    const version = message.version;
    if (this.isDisposed) {
      return this.createResponseError(message.callRendererFunction?.functionCallId ?? "unknown", RpcErrorCode.DISPOSED, "RpcHandler has been disposed.", version);
    }
    if (!message.callRendererFunction?.callFunction) {
      return this.createResponseError(message.callRendererFunction?.functionCallId ?? "unknown", RpcErrorCode.INVALID_FUNCTION_CALL, "Malformed message: missing callRendererFunction or callFunction.", version);
    }
    return null;
  }
  /**
   * Resolves the catalog and function implementation for the requested call.
   *
   * @param catalogId Optional catalog identifier specified in the call.
   * @param call Function name to resolve.
   * @param context Optional data context providing a default surface catalog.
   * @param expectedVersion Optional protocol version to check compatibility against.
   * @returns The resolved function implementation and catalog, or an error descriptor.
   */
  resolveFunctionImplementation(catalogId, call, context, expectedVersion) {
    let catalog;
    if (catalogId) {
      catalog = context?.surface?.availableCatalogs?.get(catalogId) ?? this.catalogs.find((c5) => c5.id === catalogId);
      if (!catalog) {
        return { error: `Catalog not found: ${catalogId}` };
      }
    }
    if (!catalog) {
      return { error: "No catalog available for function resolution." };
    }
    if (catalog.protocolVersion && expectedVersion) {
      if (!isCatalogVersionCompatible(catalog.protocolVersion, expectedVersion)) {
        return {
          error: `Catalog '${catalog.id}' specification version (${catalog.protocolVersion}) does not match message protocol version (${expectedVersion}).`
        };
      }
    }
    const funcImpl = catalog.functions?.get(call);
    if (!funcImpl) {
      return { error: `Function not found: ${call}` };
    }
    return { funcImpl, catalog };
  }
  /**
   * Ensures a valid DataContext is available for dynamic argument resolution and function execution.
   *
   * @param context Optional context supplied by the caller.
   * @param catalog Resolved target catalog.
   * @param functionCallId Function call identifier for fallback surface naming.
   * @returns A DataContext backed by the caller's surface or an isolated empty fallback surface.
   */
  ensureDataContext(context, catalog, functionCallId) {
    if (context instanceof DataContext && context.surface) {
      return context;
    }
    const availableCatalogs = /* @__PURE__ */ new Map();
    for (const cat of this.catalogs) {
      if (catalog.protocolVersion && cat.protocolVersion && isCatalogVersionCompatible(catalog.protocolVersion, cat.protocolVersion)) {
        availableCatalogs.set(cat.id, cat);
      }
    }
    availableCatalogs.set(catalog.id, catalog);
    const fallbackSurface = new SurfaceModel(`_rpc_fallback_${functionCallId || "default"}`, catalog, availableCatalogs);
    return new DataContext(fallbackSurface, "/");
  }
  /**
   * Resolves dynamic values (`@path` bindings and `@call` expressions) inside function arguments.
   *
   * @param args Raw arguments dictionary from the call payload.
   * @param context DataContext used to evaluate dynamic expressions.
   * @param isUserActivated Whether execution occurs in an active user gesture context.
   * @returns The resolved arguments dictionary, or an error descriptor if expression evaluation fails.
   */
  resolveDynamicArguments(args, context, isUserActivated) {
    if (args === void 0 || typeof args !== "object" || args === null || Array.isArray(args)) {
      return { args };
    }
    const resolvedArgs = {};
    let expressionError;
    const errorSub = context.surface?.onError.subscribe((err) => {
      if (!expressionError && err?.code === "EXPRESSION_ERROR") {
        expressionError = err.message || "An error occurred during function execution.";
      }
    });
    try {
      for (const [key, val] of Object.entries(args)) {
        resolvedArgs[key] = context.resolveDynamicValue(val, 1, isUserActivated);
      }
    } catch (err) {
      expressionError = this.extractErrorMessage(err) || "An error occurred during function execution.";
    } finally {
      errorSub?.unsubscribe();
    }
    if (expressionError) {
      return { error: expressionError };
    }
    return { args: resolvedArgs };
  }
  /**
   * Verifies caller boundary permissions and user activation requirements for a function.
   *
   * @param funcImpl Function implementation to inspect.
   * @param call Function name being invoked.
   * @param isUserActivated Whether execution occurs within an active user gesture context.
   * @returns An error message if permissions are not satisfied; otherwise `null`.
   */
  checkExecutionPermissions(funcImpl, call, isUserActivated) {
    const boundary = funcImpl.allowedCallers ?? "rendererOnly";
    const isAllowedCaller = boundary === "rendererOrAgent" || boundary === "agentOnly";
    if (!isAllowedCaller) {
      return `Function '${call}' cannot be called by agent (allowedCallers is ${boundary}).`;
    }
    if (funcImpl.requiresUserActivation && !isUserActivated) {
      return `Function '${call}' requires user activation context to execute.`;
    }
    return null;
  }
  /**
   * Validates and parses function arguments against the catalog function schema.
   *
   * @param catalog Catalog declaring the function.
   * @param _funcImpl Function implementation.
   * @param args Raw argument dictionary.
   * @param call Function name being invoked.
   * @returns Parsed arguments on success, or an error descriptor on validation failure.
   */
  parseArguments(catalog, _funcImpl, args, call) {
    try {
      const parsed = new PayloadValidator(catalog, { allowUnknownElements: false }).validateFunction(call, args);
      return { args: parsed };
    } catch (err) {
      const errMsg = this.extractErrorMessage(err);
      return { error: `Invalid function arguments for '${call}': ${errMsg}` };
    }
  }
  /**
   * Executes a catalog function implementation, catching unhandled exceptions and formatting the response.
   *
   * Unwraps reactive signals and maps any thrown errors to an `EXECUTION_ERROR` response.
   *
   * @param funcImpl Function implementation to execute.
   * @param args Validated function arguments.
   * @param context Data context for function evaluation.
   * @param functionCallId Identifier of the function call.
   * @param version Protocol version for the response message.
   * @returns A promise resolving to the renderer function response message.
   */
  async executeFunctionSafely(funcImpl, args, context, functionCallId, version) {
    const respVersion = version || "1.0";
    try {
      const rawResult = await Promise.resolve(funcImpl.execute(args, context));
      const unwrapped = isSignal(rawResult) ? getValue(rawResult) : rawResult;
      const value = unwrapped !== void 0 ? unwrapped : null;
      return {
        version: respVersion,
        rendererFunctionResponse: {
          functionCallId,
          value
        }
      };
    } catch (err) {
      const errMsg = this.extractErrorMessage(err);
      return {
        version: respVersion,
        rendererFunctionResponse: {
          functionCallId,
          error: {
            code: RpcErrorCode.EXECUTION_ERROR,
            message: errMsg || "An error occurred during function execution."
          }
        }
      };
    }
  }
  /**
   * Generates a unique function call identifier.
   *
   * @returns A unique call identifier string.
   */
  generateFunctionCallId() {
    if (typeof globalThis.crypto?.randomUUID === "function") {
      return globalThis.crypto.randomUUID();
    }
    const randSuffix = Math.random().toString(36).substring(2, 15).padEnd(13, "0");
    return `call-${Date.now()}-${randSuffix}`;
  }
  /**
   * Dispatches an outbound agent function call and registers a pending promise with timeout handling.
   *
   * @template T Expected return value type.
   * @param surfaceId Identifier of the surface initiating the call.
   * @param callInfo Normalized agent call parameters.
   * @returns A promise resolving to the agent function return value.
   */
  dispatchAgentCall(surfaceId, callInfo) {
    const { functionCallId, call, effectiveTimeoutMs, version } = callInfo;
    return new Promise((resolve, reject) => {
      let timer;
      const cleanup = () => {
        if (timer) {
          clearTimeout(timer);
          timer = void 0;
        }
        this.pendingAgentCalls.delete(functionCallId);
      };
      const callName = (call["@call"] ?? call.call) || "unknown";
      timer = this.createTimeoutTimer(functionCallId, callName, effectiveTimeoutMs, cleanup, reject);
      this.pendingAgentCalls.set(functionCallId, {
        resolve: (val) => {
          cleanup();
          resolve(val);
        },
        reject: (err) => {
          cleanup();
          reject(err);
        }
      });
      this.sendOutboundMessage(surfaceId, functionCallId, call, version, cleanup, reject);
    });
  }
  /**
   * Creates a timeout timer for a pending agent function call.
   *
   * @param functionCallId Identifier of the pending function call.
   * @param callName Name of the function being called.
   * @param timeoutMs Duration in milliseconds before timeout expiry.
   * @param cleanup Teardown callback to invoke when the timer fires.
   * @param reject Rejection callback to reject the pending promise with a `TIMEOUT` error.
   * @returns The timer handle, or `undefined` if timeout is disabled.
   */
  createTimeoutTimer(functionCallId, callName, timeoutMs, cleanup, reject) {
    if (timeoutMs <= 0)
      return void 0;
    return setTimeout(() => {
      if (this.pendingAgentCalls.delete(functionCallId)) {
        cleanup();
        reject(new A2uiRpcError(`Agent function call '${callName}' timed out after ${timeoutMs}ms.`, RpcErrorCode.TIMEOUT, functionCallId));
      }
    }, timeoutMs);
  }
  /**
   * Transmits an outbound callAgentFunction message to the registered outbound listener.
   *
   * @param surfaceId Identifier of the surface initiating the call.
   * @param functionCallId Identifier of the function call.
   * @param call Function call specification.
   * @param version Optional protocol version string.
   * @param cleanup Teardown callback invoked on failure.
   * @param reject Rejection callback invoked if transmission throws or rejects.
   */
  sendOutboundMessage(surfaceId, functionCallId, call, version, cleanup, reject) {
    const { call: legacyCallName, "@call": callName, ...rest } = call;
    const callFunction = { "@call": callName ?? legacyCallName, ...rest };
    const outboundMsg = {
      version: version || "1.0",
      callAgentFunction: {
        surfaceId,
        functionCallId,
        callFunction
      }
    };
    try {
      const result = this.outboundListener(outboundMsg);
      if (result && typeof result.catch === "function") {
        result.catch((err) => {
          cleanup();
          reject(err);
        });
      }
    } catch (err) {
      cleanup();
      reject(err);
    }
  }
  /**
   * Extracts a human-readable message string from an unknown error value.
   *
   * @param err Unknown error caught in a try/catch block.
   * @returns Formatted error message string.
   */
  extractErrorMessage(err) {
    return err instanceof Error ? err.message : String(err);
  }
  /**
   * Constructs a renderer function error response message.
   *
   * @param functionCallId Identifier of the function call that failed.
   * @param code RPC error code or string category.
   * @param message Human-readable error description.
   * @param version Protocol version string.
   * @returns Formatted renderer function response message containing the error.
   */
  createResponseError(functionCallId, code, message, version) {
    return {
      version: version || "1.0",
      rendererFunctionResponse: {
        functionCallId,
        error: { code, message }
      }
    };
  }
};

// node_modules/@a2ui/web_core/src/processing/message-processor.js
var MessageProcessor = class {
  /**
   * Initializes a new `MessageProcessor` instance.
   *
   * @param catalogs List of available component catalogs.
   * @param actionHandler Global handler for actions dispatched from all surfaces.
   * @param options Configuration options for the processor.
   */
  constructor(catalogs, actionHandler, options) {
    this.actionHandler = actionHandler;
    this.graphBounds = /* @__PURE__ */ new WeakMap();
    this.registeredCatalogs = catalogs;
    this.model = new SurfaceGroupModel();
    this.adapterRegistry = options?.adapterRegistry ?? defaultVersionAdapterFactory;
    this.defaultVersion = options?.version;
    this.errorHandler = options?.errorHandler;
    this.reportErrorsOnly = options?.reportErrorsOnly ?? false;
    this.rpc = new RpcHandler({
      catalogs: this.registeredCatalogs,
      outboundListener: options?.outboundListener,
      defaultTimeoutMs: options?.defaultTimeoutMs
    });
    this.validationConfig = options?.validationConfig ? withStrictDefaults(options.validationConfig) : void 0;
    if (this.actionHandler) {
      this.model.onAction.subscribe(this.actionHandler);
    }
  }
  /** The catalogs the processor was created with, in priority order. */
  get catalogs() {
    return [...this.registeredCatalogs];
  }
  /**
   * Invokes a remote function on the server agent using an options bag.
   *
   * @param surfaceId The ID of the surface requesting execution.
   * @param call The function call details.
   * @param options Optional invocation options (custom functionCallId, timeoutMs).
   * @returns A promise resolving to the agent function return value.
   */
  callAgentFunction(surfaceId, call, options) {
    return this.rpc.callAgentFunction(surfaceId, call, options);
  }
  /**
   * Disposes the MessageProcessor, tearing down active surfaces and rejecting pending RPC calls.
   */
  dispose() {
    this.rpc.dispose();
  }
  /**
   * Generates the renderer capabilities object for the current processor.
   *
   * @param options Configuration for capability generation.
   * @returns The capabilities object.
   */
  getRendererCapabilities(options) {
    const versions = options?.versions ?? (options?.version ? [options.version] : void 0);
    if (!versions || versions.length === 0) {
      throw new A2uiValidationError("At least one protocol version must be provided in CapabilitiesOptions to generate renderer capabilities.");
    }
    const result = {};
    for (const ver of versions) {
      const versionCaps = {
        supportedCatalogIds: this.registeredCatalogs.map((c5) => c5.id)
      };
      const inlineCatalogs = options?.includeInlineCatalogs ? this.registeredCatalogs.map((c5) => {
        if (compareSemVer(ver, "1.0") >= 0) {
          return this.generateInlineCatalog(c5);
        }
        return this.generateLegacyInlineCatalog(c5, options?.componentEnvelopeRef ?? "common_types.json#/$defs/ComponentCommon");
      }) : void 0;
      if (inlineCatalogs) {
        versionCaps.inlineCatalogs = inlineCatalogs;
      }
      result[ver] = versionCaps;
    }
    return result;
  }
  /**
   * Generates the client/renderer capabilities object for the current processor.
   *
   * @deprecated Use `getRendererCapabilities` instead.
   * @param options Configuration for capability generation. Defaults to
   *   `{versions: ['v0.9']}`, the only version the pre-1.0 method knew.
   * @returns The capabilities object.
   */
  getClientCapabilities(options) {
    const versions = options?.versions && options.versions.length > 0 ? options.versions : [options?.version ?? "v0.9"];
    return this.getRendererCapabilities({ ...options, versions });
  }
  /**
   * Generates a v1.0 inline catalog: the catalog's `toJson()` document with the
   * `catalogId` and bare semantic version `protocolVersion` that
   * `catalog_definition.json` requires, whatever the source document declared.
   *
   * @param catalog The catalog instance to serialize.
   * @returns The inline catalog document.
   */
  generateInlineCatalog(catalog) {
    const doc = catalog.toJson();
    doc.catalogId = catalog.id;
    const version = typeof doc.protocolVersion === "string" ? doc.protocolVersion : toCanonicalVersion(catalog.protocolVersion) ?? catalog.protocolVersion;
    doc.protocolVersion = version.trim().replace(/^[vV](?=\d)/, "");
    return doc;
  }
  /**
   * Generates a backwards-compatible inline catalog representation for v0.8/v0.9/v0.9.1.
   *
   * @param catalog The catalog instance to serialize.
   * @param componentEnvelopeRef Reference URI for the component base envelope.
   * @returns Legacy inline catalog object with array-based functions and flat theme properties.
   */
  generateLegacyInlineCatalog(catalog, componentEnvelopeRef = "common_types.json#/$defs/ComponentCommon") {
    const components = {};
    for (const [name, comp] of catalog.components.entries()) {
      let props = {};
      let reqList = [];
      if (comp.schema && typeof comp.schema === "object" && "safeParse" in comp.schema) {
        const rawZod = zodToJsonSchema(comp.schema, {
          target: "jsonSchema2019-09",
          $refStrategy: "none",
          override: exclusiveUnionOverride
        });
        cleanSchemaNode(rawZod, void 0, { stripAdditionalProperties: true });
        props = rawZod.properties || {};
        reqList = Array.isArray(rawZod.required) ? rawZod.required.filter((r8) => r8 !== "component" && r8 !== "id") : [];
      }
      const { component: _ignoredComp, id: _ignoredId, ...sanitizedProps } = props;
      components[name] = {
        allOf: [
          { $ref: componentEnvelopeRef },
          {
            properties: {
              component: { const: name },
              ...sanitizedProps
            },
            required: ["component", ...reqList]
          }
        ]
      };
    }
    const functions = [];
    for (const fn of catalog.functions.values()) {
      let paramSchema = { type: "object", properties: {} };
      if (fn.schema && typeof fn.schema === "object" && "safeParse" in fn.schema) {
        const rawZod = zodToJsonSchema(fn.schema, {
          target: "jsonSchema2019-09",
          $refStrategy: "none",
          override: exclusiveUnionOverride
        });
        cleanSchemaNode(rawZod, void 0, { stripAdditionalProperties: true });
        paramSchema = rawZod;
      }
      functions.push({
        name: fn.name,
        ...fn.description ? { description: fn.description } : {},
        returnType: fn.returnType,
        parameters: paramSchema
      });
    }
    let theme;
    if (catalog.themeSchema) {
      const rawTheme = zodToJsonSchema(catalog.themeSchema, {
        target: "jsonSchema2019-09",
        $refStrategy: "none",
        override: exclusiveUnionOverride
      });
      cleanSchemaNode(rawTheme, void 0, { stripAdditionalProperties: true });
      theme = rawTheme.properties || void 0;
    }
    return {
      catalogId: catalog.id,
      ...Object.keys(components).length > 0 ? { components } : {},
      ...functions.length > 0 ? { functions } : {},
      ...theme ? { theme } : {}
    };
  }
  /**
   * Serializes active surface data models configured for client-to-agent reporting.
   *
   * If version is provided, returns data models only for surfaces compatible with that version.
   * If version is omitted:
   *   - Automatically derives the protocol version from the active surface(s) with sendDataModel.
   *   - If active surfaces have conflicting protocol versions, throws A2uiValidationError.
   *
   * @param version Optional target protocol version to filter surfaces.
   * @returns Serialized data model payload, or undefined if no matching surfaces exist.
   */
  getRendererDataModel(version) {
    const enabledSurfaces = Array.from(this.model.surfacesMap.values()).filter((s5) => s5.sendDataModel);
    if (enabledSurfaces.length === 0) {
      return void 0;
    }
    if (version !== void 0) {
      const surfaces2 = {};
      for (const surface of enabledSurfaces) {
        const catVer = surface.protocolVersion;
        if (!catVer || isCatalogVersionCompatible(catVer, version)) {
          surfaces2[surface.id] = surface.dataModel.get("/");
        }
      }
      if (Object.keys(surfaces2).length === 0) {
        return void 0;
      }
      return {
        version,
        surfaces: surfaces2
      };
    }
    const versionsSet = /* @__PURE__ */ new Set();
    for (const surface of enabledSurfaces) {
      if (surface.protocolVersion) {
        const canonical = toCanonicalVersion(surface.protocolVersion);
        const canonicalVer = canonical ? compareSemVer(canonical, "1.0") >= 0 ? canonical : `v${canonical}` : surface.protocolVersion;
        versionsSet.add(canonicalVer);
      }
    }
    if (versionsSet.size > 1) {
      throw new A2uiValidationError(`Multiple protocol versions detected among active surfaces: ${Array.from(versionsSet).sort().join(", ")}. Specify a target protocol version in getRendererDataModel(version).`);
    }
    const verStr = versionsSet.size === 1 ? versionsSet.values().next().value : "1.0";
    const surfaces = {};
    for (const surface of enabledSurfaces) {
      surfaces[surface.id] = surface.dataModel.get("/");
    }
    return {
      version: verStr,
      surfaces
    };
  }
  /**
   * Aggregates active client/renderer data models.
   *
   * @deprecated Use `getRendererDataModel` instead.
   * @param version Optional target protocol version to format data models for.
   * @returns Serialized data model payload, or undefined if no matching surfaces exist.
   */
  getClientDataModel(version) {
    return this.getRendererDataModel(version);
  }
  /**
   * Resolves a relative path against a context path.
   *
   * @deprecated Use `DataModel.resolvePath` instead.
   * @param path The path to resolve.
   * @param contextPath The base path (optional).
   * @returns The resolved absolute path.
   */
  resolvePath(path, contextPath) {
    return DataModel.resolvePath(path, contextPath);
  }
  /**
   * Gets a read-only map of active surfaces managed by this processor.
   *
   * @returns Map of surface models keyed by surface identifier.
   */
  getSurfaces() {
    return this.model.surfacesMap;
  }
  /**
   * Retrieves an active surface by its ID.
   *
   * @param id The surface ID.
   * @returns The matching surface model, or undefined if not found.
   */
  getSurface(id) {
    return this.model.getSurface(id);
  }
  /**
   * Subscribes to surface creation events.
   *
   * @param handler Callback invoked when a surface is created.
   * @returns A subscription object to unsubscribe.
   */
  onSurfaceCreated(handler) {
    return this.model.onSurfaceCreated.subscribe(handler);
  }
  /**
   * Subscribes to surface deletion events.
   *
   * @param handler Callback invoked when a surface is deleted.
   * @returns A subscription object to unsubscribe.
   */
  onSurfaceDeleted(handler) {
    return this.model.onSurfaceDeleted.subscribe(handler);
  }
  /**
   * Processes a list of messages, a message wrapper, or raw operations synchronously.
   *
   * @param messages The messages or operations to process.
   * @param context Contextual execution options.
   */
  processMessages(messages, context) {
    this.withValidationConfig(context, () => {
      const initialContext = extractEnvelopeErrorContext(Array.isArray(messages) ? messages[0] : messages, this.defaultVersion);
      let currentContext = initialContext;
      try {
        const operations = this.prepareOperations(messages, (ctx) => {
          currentContext = ctx;
        });
        for (const op of operations) {
          currentContext = this.contextFromOperation(op, initialContext);
          this.processOperation(op, context);
        }
      } catch (err) {
        this.handleProcessingError(err, currentContext);
        if (!this.reportErrorsOnly) {
          throw err;
        }
      }
    });
  }
  /**
   * Processes one or more messages, catching any validation or processing errors
   * and returning them as spec-compliant outbound `RendererErrorMessage` objects instead
   * of throwing.
   *
   * @param messages Array of messages or single message payload to process.
   * @param context Optional execution context.
   * @returns Array of `RendererErrorMessage` envelopes produced by failed messages.
   */
  processMessagesWithErrors(messages, context) {
    return this.withValidationConfig(context, () => {
      const items = Array.isArray(messages) ? messages : [messages];
      const errors = [];
      for (const item of items) {
        const itemEnvelopeContext = extractEnvelopeErrorContext(item, this.defaultVersion);
        let currentContext = itemEnvelopeContext;
        try {
          const operations = this.prepareOperations(item, (ctx) => {
            currentContext = ctx;
          });
          for (const op of operations) {
            currentContext = this.contextFromOperation(op, itemEnvelopeContext);
            this.processOperation(op, context);
          }
        } catch (err) {
          errors.push(this.handleProcessingError(err, currentContext));
        }
      }
      return errors;
    });
  }
  /**
   * Asynchronously processes messages, executing RPC calls and returning all produced responses.
   *
   * @param messages The messages or operations to process.
   * @param context Contextual execution options.
   * @returns Array of rendererFunctionResponse messages produced during execution.
   */
  async processMessagesAsync(messages, context) {
    const initialContext = extractEnvelopeErrorContext(Array.isArray(messages) ? messages[0] : messages, this.defaultVersion);
    let currentContext = initialContext;
    try {
      const operations = this.withValidationConfig(context, () => this.prepareOperations(messages, (ctx) => {
        currentContext = ctx;
      }));
      const responses = [];
      for (const op of operations) {
        currentContext = this.contextFromOperation(op, initialContext);
        const resp = await this.processOperationAsync(op, context);
        if (resp) {
          responses.push(resp);
        }
      }
      return responses;
    } catch (err) {
      this.handleProcessingError(err, currentContext);
      if (!this.reportErrorsOnly) {
        throw err;
      }
      return [];
    }
  }
  contextFromOperation(op, fallback) {
    const opContext = op.envelopeContext ?? fallback;
    const surfaceId = "surfaceId" in op && op.surfaceId ? op.surfaceId : opContext.surfaceId;
    const functionCallId = "functionCallId" in op && op.functionCallId ? op.functionCallId : opContext.functionCallId;
    const opVersion = "version" in op ? op.version : void 0;
    const surfaceVersion = surfaceId ? this.model.getSurface(surfaceId)?.protocolVersion : void 0;
    return {
      surfaceId,
      functionCallId,
      actionType: op.type,
      version: opVersion ?? surfaceVersion ?? opContext.version ?? this.defaultVersion
    };
  }
  handleProcessingError(err, context) {
    const errorMsg = mapErrorToRendererMessage(err, context);
    const payload = {
      code: errorMsg.error.code,
      message: errorMsg.error.message,
      surfaceId: errorMsg.error.surfaceId,
      details: err instanceof A2uiValidationError ? err.details : void 0
    };
    const surface = errorMsg.error.surfaceId ? this.model.getSurface(errorMsg.error.surfaceId) : void 0;
    if (surface) {
      surface.dispatchError(payload);
    } else {
      this.model.emitError(payload);
    }
    this.errorHandler?.(errorMsg);
    return errorMsg;
  }
  extractSingleMessageOperations(item) {
    const envelopeContext = extractEnvelopeErrorContext(item, this.defaultVersion);
    let adapter;
    try {
      adapter = this.adapterRegistry.resolveFromPayload(item);
    } catch (err) {
      if (this.isImplicitV09Payload(item)) {
        const hasV091 = this.catalogs.some((c5) => c5.protocolVersion === "0.9.1" || c5.protocolVersion === "v0.9.1");
        adapter = this.adapterRegistry.getAdapter(hasV091 ? "v0.9.1" : "v0.9");
      } else {
        throw err;
      }
    }
    const itemOps = adapter.extractOperations(item);
    const itemVer = envelopeContext.version;
    for (const op of itemOps) {
      if (itemVer && toCanonicalVersion(itemVer)) {
        if (!("version" in op && op.version)) {
          op.version = itemVer;
        }
      }
      op.envelopeContext = envelopeContext;
    }
    return itemOps;
  }
  /**
   * Runs `fn` under the validation config of `context`, if it has one.
   *
   * `fn` must be synchronous: the config of a call is in effect only until
   * `fn` returns, so an asynchronous call applies it around each synchronous
   * step.
   */
  withValidationConfig(context, fn) {
    const config = context?.validationConfig;
    if (!config)
      return fn();
    const own = this.validationConfig;
    this.validationConfig = withStrictDefaults(config);
    try {
      return fn();
    } finally {
      this.validationConfig = own;
    }
  }
  prepareSingleItem(item, onItemContext) {
    onItemContext?.(extractEnvelopeErrorContext(item, this.defaultVersion));
    if (this.validationConfig) {
      validateRecursionAndPaths(item);
    }
    if (this.validationConfig?.targetVersion) {
      this.validateTargetVersion(item);
    }
    if (isInternalOperation(item)) {
      return [item];
    }
    return this.extractSingleMessageOperations(item);
  }
  prepareOperations(messages, onItemContext) {
    if (!messages || Array.isArray(messages) && messages.length === 0)
      return [];
    if (Array.isArray(messages)) {
      return messages.flatMap((item) => this.prepareSingleItem(item, onItemContext));
    }
    return this.prepareSingleItem(messages, onItemContext);
  }
  /**
   * Whether `messages` is a v0.9 payload that omits its `version`.
   *
   * Pre-1.0 renderers accepted such payloads, and agents written against them
   * still send them. The fallback applies only when every registered catalog
   * is pre-1.0 and validation is off, so a processor that handles v1.0
   * traffic keeps rejecting unversioned messages.
   */
  isImplicitV09Payload(messages) {
    if (this.validationConfig || this.catalogs.length === 0 || this.catalogs.some((c5) => isAtLeastVersion(c5.protocolVersion, "1.0"))) {
      return false;
    }
    let item = Array.isArray(messages) ? messages[0] : messages;
    if (typeof item === "object" && item !== null && "messages" in item && Array.isArray(item.messages)) {
      item = item.messages[0];
    }
    if (typeof item !== "object" || item === null || "version" in item) {
      return false;
    }
    return ["createSurface", "updateComponents", "updateDataModel", "deleteSurface"].some((key) => key in item);
  }
  validateTargetVersion(messages) {
    const expected = this.validationConfig?.targetVersion;
    if (!expected)
      return;
    const checkMsg = (msg) => {
      if (typeof msg === "object" && msg !== null && "version" in msg) {
        const msgVer = msg.version;
        if (msgVer) {
          const normMsg = toCanonicalVersion(msgVer) ?? msgVer;
          const normExpected = toCanonicalVersion(expected) ?? expected;
          if (normMsg !== normExpected) {
            throw new A2uiValidationError(`Message version '${msgVer}' does not match expected target version '${expected}'`);
          }
        }
      }
    };
    if (Array.isArray(messages)) {
      for (const m4 of messages) {
        checkMsg(m4);
      }
    } else {
      checkMsg(messages);
    }
  }
  applyStateOperation(op) {
    if (this.validationConfig?.allowedMessages && !this.validationConfig.allowedMessages.includes(op.type)) {
      throw new A2uiValidationError(`Operation '${op.type}' is not permitted by ValidationConfig.allowedMessages`);
    }
    switch (op.type) {
      case "createSurface":
        this.processCreateSurfaceOp(op);
        break;
      case "deleteSurface":
        this.processDeleteSurfaceOp(op);
        break;
      case "updateComponents":
        this.processUpdateComponentsOp(op);
        break;
      case "updateDataModel":
        this.processUpdateDataModelOp(op);
        break;
      case "agentFunctionResponse":
        this.rpc.handleAgentFunctionResponse({
          version: op.version ?? "1.0",
          agentFunctionResponse: {
            functionCallId: op.functionCallId,
            value: op.value,
            error: op.error
          }
        });
        break;
      case "callRendererFunction":
        break;
    }
  }
  /**
   * Resolves the DataContext for an inbound RPC callRendererFunction operation.
   *
   * @param op Operation containing optional surface, catalog, and function call identifiers.
   * @returns A DataContext attached to the requested surface, an isolated fallback surface when surfaceId is omitted, or undefined if surfaceId is not found.
   */
  resolveRpcDataContext(op) {
    if (op.surfaceId !== void 0) {
      const surface = this.model.surfacesMap.get(op.surfaceId);
      return surface ? new DataContext(surface, "/") : void 0;
    }
    const targetCatalog = op.catalogId ? this.registeredCatalogs.find((c5) => c5.id === op.catalogId) : void 0;
    if (!targetCatalog) {
      return void 0;
    }
    const fallbackSurfaceId = `_rpc_fallback_${op.functionCallId || "default"}`;
    const availableCatalogs = /* @__PURE__ */ new Map();
    for (const cat of this.registeredCatalogs) {
      if (targetCatalog?.protocolVersion && cat.protocolVersion && isCatalogVersionCompatible(targetCatalog.protocolVersion, cat.protocolVersion)) {
        availableCatalogs.set(cat.id, cat);
      }
    }
    if (targetCatalog) {
      availableCatalogs.set(targetCatalog.id, targetCatalog);
    }
    return new DataContext(new SurfaceModel(fallbackSurfaceId, targetCatalog, availableCatalogs), "/");
  }
  /**
   * Processes a single canonical internal operation.
   *
   * @param op The internal operation to execute.
   * @param context Contextual execution options.
   */
  processOperation(op, context) {
    if (op.type === "callRendererFunction") {
      const dataContext = this.resolveRpcDataContext(op);
      const isUserActivated = context?.isUserActivated ?? op.isUserActivated ?? false;
      const callMsg = {
        version: op.version ?? "1.0",
        callRendererFunction: {
          ...op.surfaceId !== void 0 ? { surfaceId: op.surfaceId } : {},
          functionCallId: op.functionCallId,
          callFunction: {
            "@call": op.call,
            catalogId: op.catalogId,
            args: op.args
          }
        }
      };
      void this.rpc.handleCallRendererFunction(callMsg, dataContext, isUserActivated).catch((err) => {
        console.error(`Unhandled error in callRendererFunction (${op.call}):`, err);
      });
      return;
    }
    this.withValidationConfig(context, () => this.applyStateOperation(op));
  }
  async processOperationAsync(op, context) {
    if (op.type === "callRendererFunction") {
      const dataContext = this.resolveRpcDataContext(op);
      const isUserActivated = context?.isUserActivated ?? op.isUserActivated ?? false;
      const callMsg = {
        version: op.version ?? "1.0",
        callRendererFunction: {
          ...op.surfaceId !== void 0 ? { surfaceId: op.surfaceId } : {},
          functionCallId: op.functionCallId,
          callFunction: {
            "@call": op.call,
            catalogId: op.catalogId,
            args: op.args
          }
        }
      };
      return await this.rpc.handleCallRendererFunction(callMsg, dataContext, isUserActivated);
    }
    this.withValidationConfig(context, () => this.applyStateOperation(op));
    return null;
  }
  validateSurfaceTheme(theme, catalog, surfaceId) {
    if (!theme || !catalog?.themeSchema) {
      return theme;
    }
    try {
      return new PayloadValidator(catalog, this.validationConfig).validateTheme(theme);
    } catch (err) {
      if (err instanceof A2uiValidationError) {
        const details = Array.isArray(err.details) ? err.details.map((d4) => typeof d4 === "object" && d4 !== null ? { ...d4, surfaceId } : d4) : err.details;
        throw new A2uiValidationError(err.message.replace(/^Validation failed for theme:/, `Validation failed for theme on surface '${surfaceId}':`), details, err.code, { path: err.path, surfaceId });
      }
      throw err;
    }
  }
  processCreateSurfaceOp(op) {
    const { surfaceId, catalogId, theme, sendDataModel, components, dataModel } = op;
    const msgVersion = op.version;
    const noDefaultCatalog = catalogId === void 0 && msgVersion !== void 0 && isAtLeastVersion(msgVersion, "1.0");
    const catalog = noDefaultCatalog ? void 0 : resolveSurfaceDefaultCatalog(this.registeredCatalogs, catalogId, msgVersion);
    if (!catalog && !noDefaultCatalog) {
      if (catalogId !== void 0) {
        throw new A2uiCatalogError(`Catalog not found: ${catalogId}`, "CATALOG_ERROR", { surfaceId });
      }
      throw new A2uiCatalogError(`No default catalog available for surface '${surfaceId}'.`, "CATALOG_ERROR", { surfaceId });
    }
    if (catalog?.protocolVersion && msgVersion && !isCatalogVersionCompatible(catalog.protocolVersion, msgVersion)) {
      const mismatchMsg = `Surface '${surfaceId}' catalog '${catalogId ?? catalog.id}' specification version (${catalog.protocolVersion}) does not match message protocol version (${msgVersion}).`;
      if (isAtLeastVersion(msgVersion, "1.0")) {
        throw new A2uiCatalogError(mismatchMsg, "CATALOG_ERROR", { surfaceId });
      }
      throw new A2uiValidationError(mismatchMsg, void 0, "VALIDATION_ERROR", { surfaceId });
    }
    if (this.model.getSurface(surfaceId)) {
      throw new A2uiIntegrityError(`Surface ${surfaceId} already exists.`, void 0, "INTEGRITY_ERROR", { surfaceId });
    }
    const validatedTheme = this.validateSurfaceTheme(theme, catalog, surfaceId);
    const surfaceVersion = catalog?.protocolVersion ?? msgVersion;
    const availableCatalogs = new Map(this.registeredCatalogs.filter((c5) => isCatalogVersionCompatible(c5.protocolVersion, surfaceVersion)).map((c5) => [c5.id, c5]));
    const surface = new SurfaceModel(surfaceId, catalog, availableCatalogs, validatedTheme, sendDataModel ?? false, void 0, op.rootId ?? "root", op.metadata, surfaceVersion);
    this.model.addSurface(surface);
    if (dataModel) {
      surface.dataModel.set("/", dataModel);
    }
    if (components && components.length > 0) {
      this.processUpdateComponentsOp({
        type: "updateComponents",
        surfaceId,
        components
      });
    }
  }
  processDeleteSurfaceOp(op) {
    if (!op.surfaceId)
      return;
    this.model.deleteSurface(op.surfaceId);
  }
  validateComponentProperties(comp, surface) {
    const { id, component } = comp;
    if (typeof id !== "string" || !id) {
      throw new A2uiValidationError(`Component '${component}' is missing an 'id'; entries require a valid string 'id'.`);
    }
    const targetCatalog = this.resolveComponentCatalog(comp, surface);
    if ("metadata" in comp && comp["metadata"] !== void 0) {
      validateMetadata(comp["metadata"]);
    }
    const existing = surface.componentsModel.get(id);
    const skipNestedFunctions = !this.validationConfig && !isAtLeastVersion(surface.protocolVersion, "1.0");
    new PayloadValidator(targetCatalog, this.validationConfig).validateComponent(this.componentPayloadForValidation(comp, surface, targetCatalog), existing?.type, { skipNestedFunctions });
    if (skipNestedFunctions)
      return;
    this.validateNestedFunctionCalls(comp, surface, targetCatalog);
  }
  /**
   * Builds the component payload handed to `PayloadValidator`, telling it
   * which catalog the component is in.
   *
   * From v1.0 a function call that names no `catalogId` runs in the surface
   * default catalog. The validator takes a component that names no
   * `catalogId` to be in the surface default, so the payload names the
   * component's catalog only when that catalog is another one; the validator
   * then leaves those calls to `validateNestedFunctionCalls`, which checks
   * them against the surface default. Whether the payload itself wrote a
   * `catalogId` doesn't matter: a component naming the surface default is in
   * it all the same.
   *
   * @param comp Raw component payload.
   * @param surface Surface the component belongs to.
   * @param componentCatalog Catalog the component resolved to.
   * @returns The payload to validate.
   */
  componentPayloadForValidation(comp, surface, componentCatalog) {
    const { catalogId: _catalogId, ...payload } = comp;
    if (isAtLeastVersion(surface.protocolVersion, "1.0") && componentCatalog !== surface.defaultCatalog) {
      payload["catalogId"] = componentCatalog.id;
    }
    return payload;
  }
  /**
   * Resolves the catalog that defines a component in an update.
   *
   * The component uses the catalog it names, and otherwise the surface's
   * default catalog. Below v1.0 only, an existing component updated without a
   * new type keeps its catalog; from v1.0 every update is a full replacement
   * and resolves afresh.
   *
   * @param comp Raw component payload.
   * @param surface Surface the component belongs to.
   * @returns The catalog that defines the component.
   * @throws {A2uiCatalogError} If the named catalog is unknown or speaks an
   *   incompatible protocol version, or if the component names no catalog and
   *   the surface has no default catalog.
   */
  resolveComponentCatalog(comp, surface) {
    const { id, component, catalogId } = comp;
    const subject = `Component '${id}'`;
    const atLeastV1 = isAtLeastVersion(surface.protocolVersion, "1.0");
    if (typeof catalogId === "string" && (catalogId || atLeastV1)) {
      return this.resolveSurfaceItemCatalog(surface, catalogId, subject);
    }
    if (!atLeastV1) {
      const existing = typeof id === "string" ? surface.componentsModel.get(id) : void 0;
      if (existing && (typeof component !== "string" || !component || component === existing.type)) {
        return existing.catalog;
      }
    }
    return this.resolveSurfaceItemCatalog(surface, void 0, subject);
  }
  /**
   * Validates each v1.0 function call nested in a component against the
   * catalog it resolves to on the surface.
   *
   * Each call resolves on its own, by the `catalogId` it names or else to
   * the surface's default catalog, whatever catalog an enclosing call or the
   * component names, so a call may run in a catalog other than the
   * component's. This pass resolves every call's catalog and checks the
   * arguments of the calls that run outside the component's catalog;
   * component validation, which runs first and walks every nested call, has
   * already checked the others, as decided by `nestedCallRunsInCatalog`.
   * Reserved `@` system functions belong to no catalog and are validated by
   * `PayloadValidator`; their arguments are still walked here.
   *
   * Catalog resolution runs even without a `validationConfig`, so a call no
   * catalog on the surface can run is always rejected before state changes.
   *
   * @param comp Raw component payload.
   * @param surface Surface the component belongs to.
   * @param componentCatalog Catalog the component resolved to.
   * @throws {A2uiCatalogError} If a call's catalog cannot be resolved.
   * @throws {A2uiValidationError} If a call fails validation against the
   *   catalog it resolves to.
   */
  validateSingleNestedCall(record, name, surface, componentCatalog, currentPath) {
    const rawCatalogId = record["catalogId"];
    if (rawCatalogId !== void 0 && typeof rawCatalogId !== "string") {
      throw new A2uiValidationError(nonStringCatalogIdMessage(name), void 0, "VALIDATION_ERROR", { path: currentPath || void 0, surfaceId: surface.id });
    }
    const target = this.resolveNestedCallCatalog(surface, rawCatalogId, name, currentPath);
    const checked = nestedCallRunsInCatalog(rawCatalogId, componentCatalog.id, {
      catalogIsDefault: componentCatalog === surface.defaultCatalog
    });
    if (!checked) {
      this.validateExternalCatalogFunction(target, name, record["args"], surface.id, currentPath);
    }
  }
  resolveNestedCallCatalog(surface, catalogId, name, currentPath) {
    try {
      return this.resolveSurfaceItemCatalog(surface, catalogId, `Function call '${name}'`);
    } catch (err) {
      if (err instanceof A2uiError) {
        err.surfaceId = surface.id;
        if (currentPath)
          err.path = currentPath;
      }
      throw err;
    }
  }
  validateExternalCatalogFunction(target, name, rawArgs, surfaceId, currentPath) {
    try {
      new PayloadValidator(target, this.validationConfig).validateFunction(name, rawArgs ?? {});
    } catch (err) {
      if (err instanceof A2uiError) {
        err.surfaceId = surfaceId;
        if (currentPath) {
          err.path = err.path && err.path !== "/" ? `${currentPath}${err.path}` : currentPath;
        }
      }
      throw err;
    }
  }
  validateNestedFunctionCalls(comp, surface, componentCatalog) {
    if (!isAtLeastVersion(surface.protocolVersion, "1.0"))
      return;
    const visit = (node, currentPath) => {
      if (Array.isArray(node)) {
        for (let i7 = 0; i7 < node.length; i7++) {
          visit(node[i7], `${currentPath}/${i7}`);
        }
        return;
      }
      if (typeof node !== "object" || node === null)
        return;
      const record = node;
      const name = record["@call"];
      if (typeof name === "string" && !name.startsWith("@")) {
        this.validateSingleNestedCall(record, name, surface, componentCatalog, currentPath);
      }
      for (const [k2, v3] of Object.entries(record)) {
        visit(v3, `${currentPath}/${k2}`);
      }
    };
    for (const [key, value] of Object.entries(comp)) {
      if (key === "id" || key === "component" || key === "catalogId" || key === "metadata") {
        continue;
      }
      visit(value, `/${key}`);
    }
  }
  /**
   * Resolves the catalog an item on a surface uses: the catalog it names, or
   * else the surface's default catalog.
   *
   * Delegates to `SurfaceModel.resolveCatalog`, which `DataContext` also uses
   * at runtime, so validation and execution share one rule and one set of
   * error messages. It first reports a catalog the processor knows yet the
   * surface cannot use because its protocol version differs.
   *
   * @param surface Surface the item belongs to.
   * @param catalogId Catalog the item names, if any.
   * @param subject Description of the item for error messages.
   * @returns The catalog the item resolves to.
   * @throws {A2uiCatalogError} If the item's catalog cannot be resolved.
   */
  resolveSurfaceItemCatalog(surface, catalogId, subject) {
    if (catalogId !== void 0 && !surface.availableCatalogs.has(catalogId)) {
      const known = this.registeredCatalogs.find((c5) => c5.id === catalogId);
      if (known) {
        throw new A2uiCatalogError(`${subject} names catalog '${catalogId}', whose specification version (${known.protocolVersion}) does not match surface '${surface.id}' version (${surface.protocolVersion}).`);
      }
    }
    return surface.resolveCatalog(catalogId, subject);
  }
  applyComponentUpdate(comp, surface) {
    const { id, component, catalogId: _catalogId, metadata: rawMetadata, ...properties } = comp;
    if (typeof id !== "string")
      return;
    const metadata = extractComponentMetadata(rawMetadata);
    const targetCatalog = this.resolveComponentCatalog(comp, surface);
    const existing = surface.componentsModel.get(id);
    if (existing) {
      const componentType = typeof component === "string" ? component : existing.type;
      if (componentType !== existing.type || existing.catalog.id !== targetCatalog.id) {
        surface.componentsModel.removeComponent(id);
        const newComponent = new ComponentModel(id, componentType, properties, targetCatalog, metadata);
        surface.componentsModel.addComponent(newComponent);
      } else {
        batchWrite(() => {
          existing.applyUpdate(properties, metadata);
        });
      }
    } else {
      if (typeof component !== "string" || !component) {
        throw new A2uiValidationError(`Cannot create component ${id} without a type.`);
      }
      const newComponent = new ComponentModel(id, component, properties, targetCatalog, metadata);
      surface.componentsModel.addComponent(newComponent);
    }
  }
  rebaseComponentValidationError(err, index, surfaceId) {
    if (!(err instanceof A2uiError))
      return;
    if (!err.surfaceId)
      err.surfaceId = surfaceId;
    if (!(err instanceof A2uiValidationError))
      return;
    err.path = err.path && err.path !== "/" ? `/components/${index}${err.path}` : `/components/${index}`;
    if (Array.isArray(err.details)) {
      for (const d4 of err.details) {
        if (typeof d4 === "object" && d4 !== null && !("surfaceId" in d4)) {
          d4.surfaceId = surfaceId;
        }
      }
    }
  }
  validateComponentsBatch(op, surface) {
    const seenBatchIds = /* @__PURE__ */ new Set();
    for (let i7 = 0; i7 < op.components.length; i7++) {
      const comp = op.components[i7];
      if (comp && typeof comp === "object" && typeof comp.id === "string") {
        if (seenBatchIds.has(comp.id)) {
          throw new A2uiIntegrityError(`Duplicate component ID: '${comp.id}'`, [comp.id], "INTEGRITY_ERROR", { surfaceId: op.surfaceId, path: `/components/${i7}/id` });
        }
        seenBatchIds.add(comp.id);
      }
      try {
        this.validateComponentProperties(comp, surface);
      } catch (err) {
        this.rebaseComponentValidationError(err, i7, op.surfaceId);
        throw err;
      }
    }
  }
  validateSurfaceComponentGraph(op, surface) {
    try {
      return validateComponentGraph(surface, op.components, this.validationConfig, (comp, s5) => this.resolveComponentCatalog(comp, s5), this.graphBounds);
    } catch (err) {
      if (err instanceof A2uiError && !err.surfaceId) {
        err.surfaceId = op.surfaceId;
      }
      throw err;
    }
  }
  processUpdateComponentsOp(op) {
    if (!op.surfaceId)
      return;
    const surface = this.model.getSurface(op.surfaceId);
    if (!surface) {
      throw new A2uiIntegrityError(`Surface not found for message: ${op.surfaceId}`, void 0, "INTEGRITY_ERROR", { surfaceId: op.surfaceId });
    }
    this.validateComponentsBatch(op, surface);
    const bound = this.validateSurfaceComponentGraph(op, surface);
    batchWrite(() => {
      for (const comp of op.components) {
        this.applyComponentUpdate(comp, surface);
      }
    });
    if (bound) {
      recordComponentGraphBound(surface.componentsModel, bound, this.graphBounds);
    }
  }
  processUpdateDataModelOp(op) {
    if (!op.surfaceId)
      return;
    const surface = this.model.getSurface(op.surfaceId);
    if (!surface) {
      throw new A2uiIntegrityError(`Surface not found for message: ${op.surfaceId}`, void 0, "INTEGRITY_ERROR", { surfaceId: op.surfaceId });
    }
    if (op.path !== void 0 && op.path !== "" && (!op.path.startsWith("/") || !RELAXED_PATH_PATTERN.test(op.path))) {
      const msg = `Invalid path syntax: '${op.path}' (path must be a valid RFC 6901 JSON Pointer starting with '/')`;
      throw new A2uiValidationError(msg, [{ path: "/path", code: "invalid_value", message: msg, surfaceId: op.surfaceId }], "VALIDATION_ERROR", { path: "/path", surfaceId: op.surfaceId });
    }
    const path = op.path || "/";
    const value = op.value;
    try {
      surface.dataModel.set(path, value);
    } catch (err) {
      if (err instanceof A2uiDataError && !err.surfaceId) {
        err.surfaceId = op.surfaceId;
      }
      throw err;
    }
  }
};
function extractComponentMetadata(rawMetadata) {
  if (rawMetadata && typeof rawMetadata === "object" && !Array.isArray(rawMetadata)) {
    return rawMetadata;
  }
  return void 0;
}
function resolveSurfaceDefaultCatalog(catalogs, catalogId, msgVersion) {
  if (catalogId !== void 0) {
    return catalogs.find((c5) => c5.id === catalogId);
  }
  return catalogs.find((c5) => msgVersion ? isCatalogVersionCompatible(c5.protocolVersion, msgVersion) : true) ?? catalogs[0];
}
function withStrictDefaults(config) {
  return {
    allowOrphanComponents: false,
    allowDanglingReferences: false,
    allowMissingRoot: false,
    allowUnknownElements: false,
    ...config
  };
}

// node_modules/@a2ui/web_core/src/v0_9/schema/common-types.js
var ComponentIdSchema3 = markChildRef(external_exports.string().describe("REF:#/$defs/ComponentId|The unique identifier for a component, used for both definitions and references within the same surface."), "component-id");
var DataBindingSchema3 = external_exports.object({ "path": external_exports.string().describe("A JSON Pointer path to a value in the data model.") }).strict().describe("REF:#/$defs/DataBinding");
var DynamicValueSchema3 = external_exports.union([
  external_exports.string(),
  external_exports.number(),
  external_exports.boolean(),
  external_exports.array(external_exports.any()),
  DataBindingSchema3,
  external_exports.lazy(() => FunctionCallSchema3)
]).describe("REF:#/$defs/DynamicValue|A value that can be a literal, a path, or a function call returning any type.");
var FunctionCallSchema3 = external_exports.object({
  "call": external_exports.string().describe("The name of the function to call."),
  "args": external_exports.record(external_exports.string(), external_exports.union([
    DynamicValueSchema3,
    external_exports.record(external_exports.string(), external_exports.any()).describe("A literal object argument (e.g. configuration).")
  ])).describe("Arguments passed to the function.").optional(),
  "returnType": external_exports.enum(["string", "number", "boolean", "array", "object", "any", "void"]).describe("The expected return type of the function call.").default("boolean")
}).describe("REF:#/$defs/FunctionCall|Invokes a named function on the client.");
var DynamicStringSchema3 = external_exports.union([external_exports.string(), DataBindingSchema3, FunctionCallSchema3]).describe("REF:#/$defs/DynamicString|Represents a string");
var AccessibilityAttributesSchema3 = external_exports.object({ "label": DynamicStringSchema3.optional(), "description": DynamicStringSchema3.optional() }).describe("REF:#/$defs/AccessibilityAttributes|Attributes to enhance accessibility when using assistive technologies like screen readers.");
var ComponentCommonSchema3 = external_exports.object({ "id": ComponentIdSchema3, "accessibility": AccessibilityAttributesSchema3.optional() }).describe("REF:#/$defs/ComponentCommon");
var ChildListSchema3 = markChildRef(external_exports.union([
  external_exports.array(ComponentIdSchema3).describe("A static list of child component IDs."),
  external_exports.object({
    "componentId": ComponentIdSchema3,
    "path": external_exports.string().describe("The path to the list of component property objects in the data model.")
  }).strict().describe("A template for generating a dynamic list of children from a data model list. The `componentId` is the component to use as a template.")
]).describe("REF:#/$defs/ChildList"), "child-list");
var DynamicNumberSchema3 = external_exports.union([external_exports.number(), DataBindingSchema3, FunctionCallSchema3]).describe("REF:#/$defs/DynamicNumber|Represents a value that can be either a literal number, a path to a number in the data model, or a function call returning a number.");
var DynamicBooleanSchema3 = external_exports.union([external_exports.boolean(), DataBindingSchema3, FunctionCallSchema3]).describe("REF:#/$defs/DynamicBoolean|A boolean value that can be a literal, a path, or a function call returning a boolean.");
var DynamicStringListSchema3 = external_exports.union([external_exports.array(external_exports.string()), DataBindingSchema3, FunctionCallSchema3]).describe("REF:#/$defs/DynamicStringList|Represents a value that can be either a literal array of strings, a path to a string array in the data model, or a function call returning a string array.");
var CheckRuleSchema3 = external_exports.object({
  "condition": DynamicBooleanSchema3,
  "message": external_exports.string().describe("The error message to display if the check fails.")
}).strict().describe("REF:#/$defs/CheckRule|A single validation rule applied to an input component.");
var CheckableSchema3 = external_exports.object({
  "checks": external_exports.array(CheckRuleSchema3).describe("A list of checks to perform. These are function calls that must return a boolean indicating validity.").optional()
}).describe("REF:#/$defs/Checkable|Properties for components that support client-side checks.");
var ActionSchema4 = external_exports.union([
  external_exports.object({
    "event": external_exports.object({
      "name": external_exports.string().describe("The name of the action to be dispatched to the server."),
      "context": external_exports.record(external_exports.string(), DynamicValueSchema3).describe("A JSON object containing the key-value pairs for the action context. Values can be literals or paths. Use literal values unless the value must be dynamically bound to the data model. Do NOT use paths for static IDs.").optional()
    }).strict().describe("The event to dispatch to the server.")
  }).strict().describe("Triggers a server-side event."),
  external_exports.object({ "functionCall": FunctionCallSchema3 }).strict().describe("Executes a local client-side function.")
]).describe("REF:#/$defs/Action|Defines an interaction handler that can either trigger a server-side event or execute a local client-side function.");

// node_modules/@a2ui/web_core/src/v0_9/basic_catalog/components/basic_components.js
var CatalogComponentCommonSchema = specDef("CatalogComponentCommon", external_exports.object({
  "weight": external_exports.number().describe("The relative weight of this component within a Row or Column. This is similar to the CSS 'flex-grow' property. Note: this may ONLY be set when the component is a direct descendant of a Row or Column.").optional()
}));
var ThemeDefSchema = specDef("theme", external_exports.object({
  "primaryColor": external_exports.string().regex(new RegExp("^#[0-9a-fA-F]{6}$")).describe("The primary brand color used for highlights (e.g., primary buttons, active borders). Renderers may generate variants of this color for different contexts. Format: Hexadecimal code (e.g., '#00BFFF').").optional(),
  "iconUrl": spec(external_exports.string().describe("A URL for an image that identifies the agent or tool associated with the surface."), { keywords: { format: "uri" } }).optional(),
  "agentDisplayName": external_exports.string().describe("Text to be displayed next to the surface to identify the agent or tool that created it.").optional()
}), { keywords: { additionalProperties: true } });
var TextApi = {
  name: "Text",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "text": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The text content to display. While simple Markdown formatting is supported (i.e. without HTML, images, or links), utilizing dedicated UI components is generally preferred for a richer and more structured presentation."),
    "variant": external_exports.enum(["h1", "h2", "h3", "h4", "h5", "caption", "body"]).default("body").describe("A hint for the base text style.").optional()
  }).strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema] })
};
var ImageApi = {
  name: "Image",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "url": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The URL of the image to display."),
    "description": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|Accessibility text for the image.").optional(),
    "fit": external_exports.enum(["contain", "cover", "fill", "none", "scaleDown"]).default("fill").describe("Specifies how the image should be resized to fit its container. This corresponds to the CSS 'object-fit' property.").optional(),
    "variant": external_exports.enum(["icon", "avatar", "smallFeature", "mediumFeature", "largeFeature", "header"]).default("mediumFeature").describe("A hint for the image size and style.").optional()
  }).strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema] })
};
var IconApi = {
  name: "Icon",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "name": external_exports.union([
      external_exports.enum([
        "accountCircle",
        "add",
        "arrowBack",
        "arrowForward",
        "attachFile",
        "calendarToday",
        "call",
        "camera",
        "check",
        "close",
        "delete",
        "download",
        "edit",
        "event",
        "error",
        "fastForward",
        "favorite",
        "favoriteOff",
        "folder",
        "help",
        "home",
        "info",
        "locationOn",
        "lock",
        "lockOpen",
        "mail",
        "menu",
        "moreVert",
        "moreHoriz",
        "notificationsOff",
        "notifications",
        "pause",
        "payment",
        "person",
        "phone",
        "photo",
        "play",
        "print",
        "refresh",
        "rewind",
        "search",
        "send",
        "settings",
        "share",
        "shoppingCart",
        "skipNext",
        "skipPrevious",
        "star",
        "starHalf",
        "starOff",
        "stop",
        "upload",
        "visibility",
        "visibilityOff",
        "volumeDown",
        "volumeMute",
        "volumeOff",
        "volumeUp",
        "warning"
      ]),
      spec(external_exports.object({
        "svgPath": external_exports.string()
      }), { closure: "additionalProperties" }),
      DataBindingSchema3.describe("REF:#/$defs/DataBinding")
    ]).describe("The name of the icon to display.")
  }).strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema] })
};
var VideoApi = {
  name: "Video",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "url": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The URL of the video to display.")
  }).strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema] })
};
var AudioPlayerApi = {
  name: "AudioPlayer",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "url": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The URL of the audio to be played."),
    "description": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|A description of the audio, such as a title or summary.").optional()
  }).strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema] })
};
var RowApi = {
  name: "Row",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "children": ChildListSchema3.describe("REF:#/$defs/ChildList|Defines the children. Use an array of strings for a fixed set of children, or a template object to generate children from a data list. Children cannot be defined inline, they must be referred to by ID."),
    "justify": external_exports.enum(["center", "end", "spaceAround", "spaceBetween", "spaceEvenly", "start", "stretch"]).default("start").describe("Defines the arrangement of children along the main axis (horizontally). Use 'spaceBetween' to push items to the edges, or 'start'/'end'/'center' to pack them together.").optional(),
    "align": external_exports.enum(["start", "center", "end", "stretch"]).default("stretch").describe("Defines the alignment of children along the cross axis (vertically). This is similar to the CSS 'align-items' property, but uses camelCase values (e.g., 'start').").optional()
  }).describe("A layout component that arranges its children horizontally. To create a grid layout, nest Columns within this Row.").strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema] })
};
var ColumnApi = {
  name: "Column",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "children": ChildListSchema3.describe("REF:#/$defs/ChildList|Defines the children. Use an array of strings for a fixed set of children, or a template object to generate children from a data list. Children cannot be defined inline, they must be referred to by ID."),
    "justify": external_exports.enum(["start", "center", "end", "spaceBetween", "spaceAround", "spaceEvenly", "stretch"]).default("start").describe("Defines the arrangement of children along the main axis (vertically). Use 'spaceBetween' to push items to the edges (e.g. header at top, footer at bottom), or 'start'/'end'/'center' to pack them together.").optional(),
    "align": external_exports.enum(["center", "end", "start", "stretch"]).default("stretch").describe("Defines the alignment of children along the cross axis (horizontally). This is similar to the CSS 'align-items' property.").optional()
  }).describe("A layout component that arranges its children vertically. To create a grid layout, nest Rows within this Column.").strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema] })
};
var ListApi = {
  name: "List",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "children": ChildListSchema3.describe("REF:#/$defs/ChildList|Defines the children. Use an array of strings for a fixed set of children, or a template object to generate children from a data list."),
    "direction": external_exports.enum(["vertical", "horizontal"]).default("vertical").describe("The direction in which the list items are laid out.").optional(),
    "align": external_exports.enum(["start", "center", "end", "stretch"]).default("stretch").describe("Defines the alignment of children along the cross axis.").optional()
  }).strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema] })
};
var CardApi = {
  name: "Card",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "child": ComponentIdSchema3.describe("REF:#/$defs/ComponentId|The ID of the single child component to be rendered inside the card. To display multiple elements, you MUST wrap them in a layout component (like Column or Row) and pass that container's ID here. Do NOT pass multiple IDs or a non-existent ID.")
  }).strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema] })
};
var TabsApi = {
  name: "Tabs",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "tabs": external_exports.array(spec(external_exports.object({
      "title": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The tab title."),
      "child": ComponentIdSchema3.describe("REF:#/$defs/ComponentId|The ID of the child component.")
    }), { closure: "additionalProperties" })).min(1).describe("An array of objects, where each object defines a tab with a title and a child component.")
  }).strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema] })
};
var ModalApi = {
  name: "Modal",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "trigger": ComponentIdSchema3.describe("REF:#/$defs/ComponentId|The ID of the component that opens the modal when interacted with (e.g., a button)."),
    "content": ComponentIdSchema3.describe("REF:#/$defs/ComponentId|The ID of the component to be displayed inside the modal.")
  }).strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema] })
};
var DividerApi = {
  name: "Divider",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "axis": external_exports.enum(["horizontal", "vertical"]).default("horizontal").describe("The orientation of the divider.").optional()
  }).strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema] })
};
var ButtonApi = {
  name: "Button",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "checks": external_exports.array(CheckRuleSchema3.describe("REF:#/$defs/CheckRule")).describe("A list of checks to perform. These are function calls that must return a boolean indicating validity.").optional(),
    "child": ComponentIdSchema3.describe("REF:#/$defs/ComponentId|The ID of the child component. Use a 'Text' component for a labeled button. Only use an 'Icon' if the requirements explicitly ask for an icon-only button."),
    "variant": external_exports.enum(["default", "primary", "borderless"]).default("default").describe("A hint for the button style. If omitted, a default button style is used. 'primary' indicates this is the main call-to-action button. 'borderless' means the button has no visual border or background, making its child content appear like a clickable link.").optional(),
    "action": ActionSchema4.describe("REF:#/$defs/Action")
  }).strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema, "Checkable"] })
};
var TextFieldApi = {
  name: "TextField",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "checks": external_exports.array(CheckRuleSchema3.describe("REF:#/$defs/CheckRule")).describe("A list of checks to perform. These are function calls that must return a boolean indicating validity.").optional(),
    "label": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The text label for the input field."),
    "value": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The value of the text field.").optional(),
    "variant": external_exports.enum(["longText", "number", "shortText", "obscured"]).default("shortText").describe("The type of input field to display.").optional(),
    "validationRegexp": external_exports.string().describe("A regular expression used for client-side validation of the input.").optional()
  }).strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema, "Checkable"] })
};
var CheckBoxApi = {
  name: "CheckBox",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "checks": external_exports.array(CheckRuleSchema3.describe("REF:#/$defs/CheckRule")).describe("A list of checks to perform. These are function calls that must return a boolean indicating validity.").optional(),
    "label": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The text to display next to the checkbox."),
    "value": DynamicBooleanSchema3.describe("REF:#/$defs/DynamicBoolean|The current state of the checkbox (true for checked, false for unchecked).")
  }).strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema, "Checkable"] })
};
var ChoicePickerApi = {
  name: "ChoicePicker",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "checks": external_exports.array(CheckRuleSchema3.describe("REF:#/$defs/CheckRule")).describe("A list of checks to perform. These are function calls that must return a boolean indicating validity.").optional(),
    "label": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The label for the group of options.").optional(),
    "variant": external_exports.enum(["multipleSelection", "mutuallyExclusive"]).default("mutuallyExclusive").describe("A hint for how the choice picker should be displayed and behave.").optional(),
    "options": external_exports.array(spec(external_exports.object({
      "label": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The text to display for this option."),
      "value": external_exports.string().describe("The stable value associated with this option.")
    }), { closure: "additionalProperties" })).describe("The list of available options to choose from."),
    "value": DynamicStringListSchema3.describe("REF:#/$defs/DynamicStringList|The list of currently selected values. This should be bound to a string array in the data model."),
    "displayStyle": external_exports.enum(["checkbox", "chips"]).default("checkbox").describe("The display style of the component.").optional(),
    "filterable": external_exports.boolean().default(false).describe("If true, displays a search input to filter the options.").optional()
  }).describe("A component that allows selecting one or more options from a list.").strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema, "Checkable"] })
};
var SliderApi = {
  name: "Slider",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "checks": external_exports.array(CheckRuleSchema3.describe("REF:#/$defs/CheckRule")).describe("A list of checks to perform. These are function calls that must return a boolean indicating validity.").optional(),
    "label": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The label for the slider.").optional(),
    "min": external_exports.number().default(0).describe("The minimum value of the slider.").optional(),
    "max": external_exports.number().describe("The maximum value of the slider."),
    "value": DynamicNumberSchema3.describe("REF:#/$defs/DynamicNumber|The current value of the slider.")
  }).strict(), {
    mixins: ["ComponentCommon", CatalogComponentCommonSchema, "Checkable"],
    required: ["value", "max"]
  })
};
var DateTimeInputApi = {
  name: "DateTimeInput",
  schema: spec(external_exports.object({
    ...CatalogComponentCommonSchema.shape,
    "accessibility": AccessibilityAttributesSchema3.describe("REF:#/$defs/AccessibilityAttributes").optional(),
    "checks": external_exports.array(CheckRuleSchema3.describe("REF:#/$defs/CheckRule")).describe("A list of checks to perform. These are function calls that must return a boolean indicating validity.").optional(),
    "value": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The selected date and/or time value in ISO 8601 format. If not yet set, initialize with an empty string."),
    "enableDate": external_exports.boolean().default(false).describe("If true, allows the user to select a date.").optional(),
    "enableTime": external_exports.boolean().default(false).describe("If true, allows the user to select a time.").optional(),
    "min": spec(DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The minimum allowed date/time in ISO 8601 format."), {
      allOf: [
        {
          if: { type: "string" },
          then: { oneOf: [{ format: "date" }, { format: "time" }, { format: "date-time" }] }
        }
      ]
    }).optional(),
    "max": spec(DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The maximum allowed date/time in ISO 8601 format."), {
      allOf: [
        {
          if: { type: "string" },
          then: { oneOf: [{ format: "date" }, { format: "time" }, { format: "date-time" }] }
        }
      ]
    }).optional(),
    "label": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The text label for the input field.").optional()
  }).strict(), { mixins: ["ComponentCommon", CatalogComponentCommonSchema, "Checkable"] })
};

// node_modules/@a2ui/web_core/src/v0_9/basic_catalog/functions/basic_functions_api.js
var RequiredApi = {
  name: "required",
  returnType: "boolean",
  description: "Checks that the value is not null, undefined, or empty.",
  schema: spec(external_exports.object({
    "value": external_exports.any().refine((v3) => v3 !== void 0, "Required").describe("The value to check.")
  }), { closure: "additionalProperties" })
};
var RegexApi = {
  name: "regex",
  returnType: "boolean",
  description: "Checks that the value matches a regular expression string.",
  schema: spec(external_exports.object({
    "value": DynamicStringSchema3.describe("REF:#/$defs/DynamicString"),
    "pattern": external_exports.string().describe("The regex pattern to match against.")
  }), { closure: "unevaluatedProperties" })
};
var LengthApi = {
  name: "length",
  returnType: "boolean",
  description: "Checks string length constraints.",
  schema: spec(external_exports.object({
    "value": DynamicStringSchema3.describe("REF:#/$defs/DynamicString"),
    "min": spec(external_exports.number().int().describe("The minimum allowed length."), {
      keywords: { minimum: 0 }
    }).optional(),
    "max": spec(external_exports.number().int().describe("The maximum allowed length."), {
      keywords: { minimum: 0 }
    }).optional()
  }), {
    closure: "unevaluatedProperties",
    keywords: { anyOf: [{ required: ["min"] }, { required: ["max"] }] }
  })
};
var NumericApi = {
  name: "numeric",
  returnType: "boolean",
  description: "Checks numeric range constraints.",
  schema: spec(external_exports.object({
    "value": DynamicNumberSchema3.describe("REF:#/$defs/DynamicNumber"),
    "min": external_exports.number().describe("The minimum allowed value.").optional(),
    "max": external_exports.number().describe("The maximum allowed value.").optional()
  }), {
    closure: "unevaluatedProperties",
    keywords: { anyOf: [{ required: ["min"] }, { required: ["max"] }] }
  })
};
var EmailApi = {
  name: "email",
  returnType: "boolean",
  description: "Checks that the value is a valid email address.",
  schema: spec(external_exports.object({
    "value": DynamicStringSchema3.describe("REF:#/$defs/DynamicString")
  }), { closure: "unevaluatedProperties" })
};
var FormatStringApi = {
  name: "formatString",
  returnType: "string",
  description: "Performs string interpolation of data model values and other functions in the catalog functions list and returns the resulting string. The value string can contain interpolated expressions in the `${expression}` format. Supported expression types include: JSON Pointer paths to the data model (e.g., `${/absolute/path}` or `${relative/path}`), and client-side function calls (e.g., `${now()}`). Function arguments must be named (e.g., `${formatDate(value:${/currentDate}, format:'MM-dd')}`). To include a literal `${` sequence, escape it as `\\${`.",
  schema: spec(external_exports.object({
    "value": DynamicStringSchema3.describe("REF:#/$defs/DynamicString")
  }), { closure: "unevaluatedProperties" })
};
var FormatNumberApi = {
  name: "formatNumber",
  returnType: "string",
  description: "Formats a number with the specified grouping and decimal precision.",
  schema: spec(external_exports.object({
    "value": DynamicNumberSchema3.describe("REF:#/$defs/DynamicNumber|The number to format."),
    "decimals": DynamicNumberSchema3.describe("REF:#/$defs/DynamicNumber|Optional. The number of decimal places to show. Defaults to 0 or 2 depending on locale.").optional(),
    "grouping": DynamicBooleanSchema3.describe("REF:#/$defs/DynamicBoolean|Optional. If true, uses locale-specific grouping separators (e.g. '1,000'). If false, returns raw digits (e.g. '1000'). Defaults to true.").optional()
  }), { closure: "unevaluatedProperties" })
};
var FormatCurrencyApi = {
  name: "formatCurrency",
  returnType: "string",
  description: "Formats a number as a currency string.",
  schema: spec(external_exports.object({
    "value": DynamicNumberSchema3.describe("REF:#/$defs/DynamicNumber|The monetary amount."),
    "currency": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The ISO 4217 currency code (e.g., 'USD', 'EUR')."),
    "decimals": DynamicNumberSchema3.describe("REF:#/$defs/DynamicNumber|Optional. The number of decimal places to show. Defaults to 0 or 2 depending on locale.").optional(),
    "grouping": DynamicBooleanSchema3.describe("REF:#/$defs/DynamicBoolean|Optional. If true, uses locale-specific grouping separators (e.g. '1,000'). If false, returns raw digits (e.g. '1000'). Defaults to true.").optional()
  }), { required: ["currency", "value"], closure: "unevaluatedProperties" })
};
var FormatDateApi = {
  name: "formatDate",
  returnType: "string",
  description: "Formats a timestamp into a string using a pattern.",
  schema: spec(external_exports.object({
    "value": DynamicValueSchema3.describe("REF:#/$defs/DynamicValue|The date to format."),
    "format": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|A Unicode TR35 date pattern string.\n\nToken Reference:\n- Year: 'yy' (26), 'yyyy' (2026)\n- Month: 'M' (1), 'MM' (01), 'MMM' (Jan), 'MMMM' (January)\n- Day: 'd' (1), 'dd' (01), 'E' (Tue), 'EEEE' (Tuesday)\n- Hour (12h): 'h' (1-12), 'hh' (01-12) - requires 'a' for AM/PM\n- Hour (24h): 'H' (0-23), 'HH' (00-23) - Military Time\n- Minute: 'mm' (00-59)\n- Second: 'ss' (00-59)\n- Period: 'a' (AM/PM)\n\nExamples:\n- 'MMM dd, yyyy' -> 'Jan 16, 2026'\n- 'HH:mm' -> '14:30' (Military)\n- 'h:mm a' -> '2:30 PM'\n- 'EEEE, d MMMM' -> 'Friday, 16 January'")
  }), { required: ["format", "value"], closure: "unevaluatedProperties" })
};
var PluralizeApi = {
  name: "pluralize",
  returnType: "string",
  description: "Returns a localized string based on the Common Locale Data Repository (CLDR) plural category of the count (zero, one, two, few, many, other). Requires an 'other' fallback. For English, just use 'one' and 'other'.",
  schema: spec(external_exports.object({
    "value": DynamicNumberSchema3.describe("REF:#/$defs/DynamicNumber|The numeric value used to determine the plural category."),
    "zero": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|String for the 'zero' category (e.g., 0 items).").optional(),
    "one": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|String for the 'one' category (e.g., 1 item).").optional(),
    "two": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|String for the 'two' category (used in Arabic, Welsh, etc.).").optional(),
    "few": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|String for the 'few' category (e.g., small groups in Slavic languages).").optional(),
    "many": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|String for the 'many' category (e.g., large groups in various languages).").optional(),
    "other": DynamicStringSchema3.describe("REF:#/$defs/DynamicString|The default/fallback string (used for general plural cases).")
  }), { closure: "unevaluatedProperties" })
};
var OpenUrlApi = {
  name: "openUrl",
  returnType: "void",
  description: "Opens the specified URL in a browser or handler. This function has no return value.",
  schema: spec(external_exports.object({
    "url": spec(external_exports.string().describe("The URL to open."), { keywords: { format: "uri" } })
  }), { closure: "additionalProperties" })
};
var AndApi = {
  name: "and",
  returnType: "boolean",
  description: "Performs a logical AND operation on a list of boolean values.",
  schema: spec(external_exports.object({
    "values": external_exports.array(DynamicBooleanSchema3.describe("REF:#/$defs/DynamicBoolean")).min(2).describe("The list of boolean values to evaluate.")
  }), { closure: "unevaluatedProperties" })
};
var OrApi = {
  name: "or",
  returnType: "boolean",
  description: "Performs a logical OR operation on a list of boolean values.",
  schema: spec(external_exports.object({
    "values": external_exports.array(DynamicBooleanSchema3.describe("REF:#/$defs/DynamicBoolean")).min(2).describe("The list of boolean values to evaluate.")
  }), { closure: "unevaluatedProperties" })
};
var NotApi = {
  name: "not",
  returnType: "boolean",
  description: "Performs a logical NOT operation on a boolean value.",
  schema: spec(external_exports.object({
    "value": DynamicBooleanSchema3.describe("REF:#/$defs/DynamicBoolean|The boolean value to negate.")
  }), { closure: "unevaluatedProperties" })
};

// node_modules/@a2ui/web_core/src/v0_9/basic_catalog/functions/legacy_functions_api.js
var nullableNumber = () => external_exports.preprocess((v3) => v3 === null ? void 0 : v3, external_exports.coerce.number());
var stringArg = () => external_exports.preprocess((v3) => v3 === void 0 || v3 === null ? void 0 : String(v3), external_exports.string());
var anyRequired = () => external_exports.any().refine((v3) => v3 !== void 0, "Required");
var AddApi = {
  name: "add",
  returnType: "number",
  schema: external_exports.object({ "a": nullableNumber(), "b": nullableNumber() })
};
var SubtractApi = {
  name: "subtract",
  returnType: "number",
  schema: external_exports.object({ "a": nullableNumber(), "b": nullableNumber() })
};
var MultiplyApi = {
  name: "multiply",
  returnType: "number",
  schema: external_exports.object({ "a": nullableNumber(), "b": nullableNumber() })
};
var DivideApi = {
  name: "divide",
  returnType: "number",
  schema: external_exports.object({ "a": nullableNumber(), "b": nullableNumber() })
};
var EqualsApi = {
  name: "equals",
  returnType: "boolean",
  schema: external_exports.object({ "a": anyRequired(), "b": anyRequired() })
};
var NotEqualsApi = {
  name: "not_equals",
  returnType: "boolean",
  schema: external_exports.object({ "a": anyRequired(), "b": anyRequired() })
};
var GreaterThanApi = {
  name: "greater_than",
  returnType: "boolean",
  schema: external_exports.object({ "a": nullableNumber(), "b": nullableNumber() })
};
var LessThanApi = {
  name: "less_than",
  returnType: "boolean",
  schema: external_exports.object({ "a": nullableNumber(), "b": nullableNumber() })
};
var ContainsApi = {
  name: "contains",
  returnType: "boolean",
  schema: external_exports.object({ "string": stringArg(), "substring": stringArg() })
};
var StartsWithApi = {
  name: "starts_with",
  returnType: "boolean",
  schema: external_exports.object({ "string": stringArg(), "prefix": stringArg() })
};
var EndsWithApi = {
  name: "ends_with",
  returnType: "boolean",
  schema: external_exports.object({ "string": stringArg(), "suffix": stringArg() })
};

// node_modules/@a2ui/web_core/src/expressions/expression_parser.js
var MAX_EXPRESSION_TEMPLATE_LENGTH = 1e4;
var MAX_EXPRESSION_PARTS = 1e3;
var NUMBER_LITERAL = /^[+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$/;
var ExpressionParser = class _ExpressionParser {
  /**
   * Parses an input string into an array of DynamicValues.
   *
   * If the input contains no interpolation placeholders, returns the raw string as a single literal.
   *
   * @param input Raw string to parse.
   * @param depth Current recursion depth.
   * @returns An array of parsed dynamic values and literals.
   */
  parse(input, depth = 0) {
    if (depth > _ExpressionParser.MAX_DEPTH) {
      throw new A2uiExpressionError("Max recursion depth reached in parse");
    }
    if (input && input.length > MAX_EXPRESSION_TEMPLATE_LENGTH) {
      throw new A2uiExpressionError(`Expression template length (${input.length}) exceeds maximum limit (${MAX_EXPRESSION_TEMPLATE_LENGTH})`);
    }
    if (!input) {
      return [];
    }
    if (!input.includes("${")) {
      return [input];
    }
    const parts = [];
    const scanner = new Scanner(input);
    while (!scanner.isAtEnd()) {
      if (parts.length >= MAX_EXPRESSION_PARTS) {
        throw new A2uiExpressionError(`Expression parts count exceeds maximum limit (${MAX_EXPRESSION_PARTS})`);
      }
      if (scanner.matches("${")) {
        scanner.advance(2);
        const content = this.extractInterpolationContent(scanner);
        const parsed = this.parseExpression(content, depth + 1);
        if (parsed !== null) {
          parts.push(parsed);
        }
      } else if (scanner.peek() === "\\" && scanner.peek(1) === "$" && scanner.peek(2) === "{") {
        scanner.advance();
        parts.push("${");
        scanner.advance(2);
      } else {
        const start = scanner.pos;
        while (!scanner.isAtEnd()) {
          if (scanner.matches("${")) {
            break;
          }
          if (scanner.peek() === "\\" && scanner.peek(1) === "$" && scanner.peek(2) === "{") {
            break;
          }
          scanner.advance();
        }
        parts.push(scanner.input.substring(start, scanner.pos));
      }
    }
    return parts.filter((p5) => p5 !== null && p5 !== "");
  }
  extractInterpolationContent(scanner) {
    const start = scanner.pos;
    let braceBalance = 1;
    while (!scanner.isAtEnd() && braceBalance > 0) {
      const char = scanner.advance();
      if (char === "{") {
        braceBalance++;
      } else if (char === "}") {
        braceBalance--;
      } else if (char === "'" || char === '"') {
        const quote = char;
        while (!scanner.isAtEnd()) {
          const c5 = scanner.advance();
          if (c5 === "\\") {
            scanner.advance();
          } else if (c5 === quote) {
            break;
          }
        }
      }
    }
    if (braceBalance > 0) {
      throw new A2uiExpressionError("Unclosed interpolation: missing '}'");
    }
    return scanner.input.substring(start, scanner.pos - 1);
  }
  /**
   * Parses a single expression string into a DynamicValue.
   *
   * Unlike `parse()`, which handles mixed literal text and interpolations,
   * this assumes the entire string is a single expression (e.g., as found inside `${...}`).
   *
   * @param expr The expression string to parse.
   * @param depth The current recursion depth.
   * @returns The resolved DynamicValue.
   */
  parseExpression(expr, depth = 0) {
    expr = expr.trim();
    if (!expr)
      return "";
    const scanner = new Scanner(expr);
    const result = this.parseExpressionInternal(scanner, depth);
    if (!scanner.isAtEnd()) {
      throw new A2uiExpressionError(`Unexpected characters at end of expression: '${scanner.input.substring(scanner.pos)}'`);
    }
    return result;
  }
  parseExpressionInternal(scanner, depth) {
    if (depth > _ExpressionParser.MAX_DEPTH) {
      throw new A2uiExpressionError("Max recursion depth reached in parse");
    }
    scanner.skipWhitespace();
    if (scanner.isAtEnd())
      return "";
    if (scanner.matches("${")) {
      scanner.advance(2);
      const content = this.extractInterpolationContent(scanner);
      return this.parseExpression(content, depth + 1);
    }
    if (scanner.matchesString("'") || scanner.matchesString('"')) {
      return this.parseStringLiteral(scanner);
    }
    if (this.isNumberStart(scanner)) {
      return this.parseNumberLiteral(scanner);
    }
    if (scanner.matchesKeyword("true"))
      return true;
    if (scanner.matchesKeyword("false"))
      return false;
    if (scanner.matchesKeyword("null"))
      return "";
    const token = this.scanPathOrIdentifier(scanner);
    scanner.skipWhitespace();
    if (scanner.peek() === "(") {
      return this.parseFunctionCall(token, scanner, depth);
    } else {
      if (token.startsWith("@")) {
        throw new A2uiExpressionError(`Expected '(' after function name '${token}'`);
      }
      if (!token) {
        return "";
      }
      return { path: token };
    }
  }
  scanPathOrIdentifier(scanner) {
    const start = scanner.pos;
    if (scanner.peek() === "@") {
      const next = scanner.peek(1);
      if (!next || !/[A-Za-z_]/.test(next)) {
        throw new A2uiExpressionError("Invalid identifier starting with '@' in expression");
      }
      scanner.advance();
      while (!scanner.isAtEnd()) {
        const c5 = scanner.peek();
        if (/[A-Za-z0-9_]/.test(c5)) {
          scanner.advance();
        } else {
          break;
        }
      }
      return scanner.input.substring(start, scanner.pos);
    }
    while (!scanner.isAtEnd()) {
      const c5 = scanner.peek();
      if (isIdContinue(c5) || c5 === "/" || c5 === "." || c5 === "-") {
        scanner.advance();
      } else {
        break;
      }
    }
    return scanner.input.substring(start, scanner.pos);
  }
  parseFunctionCall(funcName, scanner, depth) {
    scanner.match("(");
    scanner.skipWhitespace();
    const args = {};
    while (!scanner.isAtEnd() && scanner.peek() !== ")") {
      const argName = this.scanIdentifier(scanner);
      scanner.skipWhitespace();
      if (!scanner.match(":")) {
        throw new A2uiExpressionError(`Expected ':' after argument name '${argName}' in function '${funcName}'`);
      }
      scanner.skipWhitespace();
      args[argName] = this.parseExpressionInternal(scanner, depth + 1);
      scanner.skipWhitespace();
      if (scanner.peek() === ",") {
        scanner.advance();
        scanner.skipWhitespace();
      }
    }
    if (!scanner.match(")")) {
      throw new A2uiExpressionError(`Expected ')' after function arguments for '${funcName}'`);
    }
    return { call: funcName, args, returnType: "any" };
  }
  scanIdentifier(scanner) {
    const start = scanner.pos;
    while (!scanner.isAtEnd() && isIdContinue(scanner.peek())) {
      scanner.advance();
    }
    return scanner.input.substring(start, scanner.pos);
  }
  parseStringLiteral(scanner) {
    const quote = scanner.advance();
    let result = "";
    let closed = false;
    while (!scanner.isAtEnd()) {
      const c5 = scanner.advance();
      if (c5 === "\\") {
        if (scanner.isAtEnd())
          break;
        const next = scanner.advance();
        if (next === "n")
          result += "\n";
        else if (next === "t")
          result += "	";
        else if (next === "r")
          result += "\r";
        else
          result += next;
      } else if (c5 === quote) {
        closed = true;
        break;
      } else {
        result += c5;
      }
    }
    if (!closed) {
      throw new A2uiExpressionError(`Unclosed string literal: missing closing ${quote}`);
    }
    return result;
  }
  /**
   * Whether the scanner is at the start of a number literal: a digit, a `.`
   * followed by a digit, or a `-` or `+` sign followed by either of those.
   *
   * The grammar has no arithmetic operators, so a sign here can only belong to
   * a literal. A `-` or `.` inside a path such as `a-1` or `a.5` never reaches
   * this check, because the path scanner consumes it as part of the token.
   */
  isNumberStart(scanner) {
    let offset = 0;
    if (scanner.peek() === "-" || scanner.peek() === "+")
      offset = 1;
    if (this.isDigit(scanner.peek(offset)))
      return true;
    return scanner.peek(offset) === "." && this.isDigit(scanner.peek(offset + 1));
  }
  parseNumberLiteral(scanner) {
    const start = scanner.pos;
    if (scanner.peek() === "-" || scanner.peek() === "+") {
      scanner.advance();
    }
    while (!scanner.isAtEnd() && (this.isDigit(scanner.peek()) || scanner.peek() === ".")) {
      scanner.advance();
    }
    this.skipExponent(scanner);
    const text = scanner.input.substring(start, scanner.pos);
    if (!NUMBER_LITERAL.test(text)) {
      throw new A2uiExpressionError(`Invalid number literal: '${text}'`);
    }
    const value = Number(text);
    if (!Number.isFinite(value)) {
      throw new A2uiExpressionError(`Number literal is out of range: '${text}'`);
    }
    return value === 0 ? 0 : value;
  }
  /**
   * Consumes an exponent suffix (`e` or `E`, an optional sign, then digits)
   * if one is present.
   *
   * A malformed exponent such as `1e` or `1e+` is still consumed, so that
   * `parseNumberLiteral` reports it as an invalid literal instead of leaving
   * trailing characters behind.
   */
  skipExponent(scanner) {
    if (scanner.isAtEnd() || scanner.peek() !== "e" && scanner.peek() !== "E") {
      return;
    }
    scanner.advance();
    if (!scanner.isAtEnd() && (scanner.peek() === "+" || scanner.peek() === "-")) {
      scanner.advance();
    }
    while (!scanner.isAtEnd() && this.isDigit(scanner.peek())) {
      scanner.advance();
    }
  }
  isDigit(c5) {
    return c5 >= "0" && c5 <= "9";
  }
};
ExpressionParser.MAX_DEPTH = 100;
var XID_CONTINUE_REGEX = new RegExp("^\\p{XID_Continue}$", "u");
function isIdContinue(input) {
  if (Array.from(input).length !== 1) {
    return false;
  }
  return XID_CONTINUE_REGEX.test(input);
}
var Scanner = class {
  constructor(input) {
    this.input = input;
    this.pos = 0;
  }
  isAtEnd() {
    return this.pos >= this.input.length;
  }
  peek(offset = 0) {
    const targetPos = this.pos + offset;
    if (targetPos >= this.input.length)
      return "\0";
    const codePoint = this.input.codePointAt(targetPos);
    if (codePoint === void 0)
      return "\0";
    return String.fromCodePoint(codePoint);
  }
  advance(count) {
    const step = count ?? (this.peek() === "\0" ? 1 : this.peek().length);
    const char = this.input.substring(this.pos, this.pos + step);
    this.pos += step;
    return char;
  }
  match(expected) {
    if (this.peek() === expected) {
      this.advance();
      return true;
    }
    return false;
  }
  matches(expected) {
    if (this.input.startsWith(expected, this.pos)) {
      return true;
    }
    return false;
  }
  matchesString(expected) {
    return this.peek() === expected;
  }
  matchesKeyword(keyword) {
    if (this.input.startsWith(keyword, this.pos)) {
      const next = this.peek(keyword.length);
      if (!isIdContinue(next)) {
        this.advance(keyword.length);
        return true;
      }
    }
    return false;
  }
  skipWhitespace() {
    while (!this.isAtEnd() && /\s/.test(this.peek())) {
      this.advance();
    }
  }
};

// node_modules/redos-detector/dist/redos-detector.mjs
var __assign = function() {
  __assign = Object.assign || function __assign2(t7) {
    for (var s5, i7 = 1, n6 = arguments.length; i7 < n6; i7++) {
      s5 = arguments[i7];
      for (var p5 in s5) if (Object.prototype.hasOwnProperty.call(s5, p5)) t7[p5] = s5[p5];
    }
    return t7;
  };
  return __assign.apply(this, arguments);
};
function __generator(thisArg, body) {
  var _3 = { label: 0, sent: function() {
    if (t7[0] & 1) throw t7[1];
    return t7[1];
  }, trys: [], ops: [] }, f4, y4, t7, g3 = Object.create((typeof Iterator === "function" ? Iterator : Object).prototype);
  return g3.next = verb(0), g3["throw"] = verb(1), g3["return"] = verb(2), typeof Symbol === "function" && (g3[Symbol.iterator] = function() {
    return this;
  }), g3;
  function verb(n6) {
    return function(v3) {
      return step([n6, v3]);
    };
  }
  function step(op) {
    if (f4) throw new TypeError("Generator is already executing.");
    while (g3 && (g3 = 0, op[0] && (_3 = 0)), _3) try {
      if (f4 = 1, y4 && (t7 = op[0] & 2 ? y4["return"] : op[0] ? y4["throw"] || ((t7 = y4["return"]) && t7.call(y4), 0) : y4.next) && !(t7 = t7.call(y4, op[1])).done) return t7;
      if (y4 = 0, t7) op = [op[0] & 2, t7.value];
      switch (op[0]) {
        case 0:
        case 1:
          t7 = op;
          break;
        case 4:
          _3.label++;
          return { value: op[1], done: false };
        case 5:
          _3.label++;
          y4 = op[1];
          op = [0];
          continue;
        case 7:
          op = _3.ops.pop();
          _3.trys.pop();
          continue;
        default:
          if (!(t7 = _3.trys, t7 = t7.length > 0 && t7[t7.length - 1]) && (op[0] === 6 || op[0] === 2)) {
            _3 = 0;
            continue;
          }
          if (op[0] === 3 && (!t7 || op[1] > t7[0] && op[1] < t7[3])) {
            _3.label = op[1];
            break;
          }
          if (op[0] === 6 && _3.label < t7[1]) {
            _3.label = t7[1];
            t7 = op;
            break;
          }
          if (t7 && _3.label < t7[2]) {
            _3.label = t7[2];
            _3.ops.push(op);
            break;
          }
          if (t7[2]) _3.ops.pop();
          _3.trys.pop();
          continue;
      }
      op = body.call(thisArg, _3);
    } catch (e7) {
      op = [6, e7];
      y4 = 0;
    } finally {
      f4 = t7 = 0;
    }
    if (op[0] & 5) throw op[1];
    return { value: op[0] ? op[1] : void 0, done: true };
  }
}
function __values(o7) {
  var s5 = typeof Symbol === "function" && Symbol.iterator, m4 = s5 && o7[s5], i7 = 0;
  if (m4) return m4.call(o7);
  if (o7 && typeof o7.length === "number") return {
    next: function() {
      if (o7 && i7 >= o7.length) o7 = void 0;
      return { value: o7 && o7[i7++], done: !o7 };
    }
  };
  throw new TypeError(s5 ? "Object is not iterable." : "Symbol.iterator is not defined.");
}
function __read(o7, n6) {
  var m4 = typeof Symbol === "function" && o7[Symbol.iterator];
  if (!m4) return o7;
  var i7 = m4.call(o7), r8, ar = [], e7;
  try {
    while ((n6 === void 0 || n6-- > 0) && !(r8 = i7.next()).done) ar.push(r8.value);
  } catch (error) {
    e7 = { error };
  } finally {
    try {
      if (r8 && !r8.done && (m4 = i7["return"])) m4.call(i7);
    } finally {
      if (e7) throw e7.error;
    }
  }
  return ar;
}
function __spreadArray(to, from, pack) {
  if (pack || arguments.length === 2) for (var i7 = 0, l5 = from.length, ar; i7 < l5; i7++) {
    if (ar || !(i7 in from)) {
      if (!ar) ar = Array.prototype.slice.call(from, 0, i7);
      ar[i7] = from[i7];
    }
  }
  return to.concat(ar || Array.prototype.slice.call(from));
}
var parser = { exports: {} };
(function(module) {
  (function() {
    var fromCodePoint = String.fromCodePoint || (function() {
      var stringFromCharCode = String.fromCharCode;
      var floor = Math.floor;
      return function fromCodePoint2() {
        var MAX_SIZE = 16384;
        var codeUnits = [];
        var highSurrogate;
        var lowSurrogate;
        var index = -1;
        var length = arguments.length;
        if (!length) {
          return "";
        }
        var result = "";
        while (++index < length) {
          var codePoint = Number(arguments[index]);
          if (!isFinite(codePoint) || // `NaN`, `+Infinity`, or `-Infinity`
          codePoint < 0 || // not a valid Unicode code point
          codePoint > 1114111 || // not a valid Unicode code point
          floor(codePoint) != codePoint) {
            throw RangeError("Invalid code point: " + codePoint);
          }
          if (codePoint <= 65535) {
            codeUnits.push(codePoint);
          } else {
            codePoint -= 65536;
            highSurrogate = (codePoint >> 10) + 55296;
            lowSurrogate = codePoint % 1024 + 56320;
            codeUnits.push(highSurrogate, lowSurrogate);
          }
          if (index + 1 == length || codeUnits.length > MAX_SIZE) {
            result += stringFromCharCode.apply(null, codeUnits);
            codeUnits.length = 0;
          }
        }
        return result;
      };
    })();
    function parse2(str, flags, features) {
      if (!features) {
        features = {};
      }
      function updateRawStart(node, start) {
        node.range[0] = start;
        node.raw = str.substring(start, node.range[1]);
        return node;
      }
      function createAnchor(kind, rawLength) {
        return {
          type: "anchor",
          kind,
          range: [
            pos - rawLength,
            pos
          ],
          raw: str.substring(pos - rawLength, pos)
        };
      }
      function createValue(kind, codePoint, from, to) {
        return {
          type: "value",
          kind,
          codePoint,
          range: [from, to],
          raw: str.substring(from, to)
        };
      }
      function createEscaped(kind, codePoint, value, fromOffset) {
        fromOffset = fromOffset || 0;
        return createValue(kind, codePoint, pos - (value.length + fromOffset), pos);
      }
      function createCharacter(matches) {
        var _char = matches[0];
        var first = _char.charCodeAt(0);
        if (isUnicodeMode) {
          var second;
          if (_char.length === 1 && first >= 55296 && first <= 56319) {
            second = lookahead().charCodeAt(0);
            if (second >= 56320 && second <= 57343) {
              pos++;
              return createValue(
                "symbol",
                (first - 55296) * 1024 + second - 56320 + 65536,
                pos - 2,
                pos
              );
            }
          }
        }
        return createValue("symbol", first, pos - 1, pos);
      }
      function createDisjunction(alternatives, from, to) {
        return {
          type: "disjunction",
          body: alternatives,
          range: [
            from,
            to
          ],
          raw: str.substring(from, to)
        };
      }
      function createDot() {
        return {
          type: "dot",
          range: [
            pos - 1,
            pos
          ],
          raw: "."
        };
      }
      function createCharacterClassEscape(value) {
        return {
          type: "characterClassEscape",
          value,
          range: [
            pos - 2,
            pos
          ],
          raw: str.substring(pos - 2, pos)
        };
      }
      function createReference(matchIndex) {
        var start = pos - 1 - matchIndex.length;
        return {
          type: "reference",
          matchIndex: parseInt(matchIndex, 10),
          range: [
            start,
            pos
          ],
          raw: str.substring(start, pos)
        };
      }
      function createNamedReference(name) {
        var start = name.range[0] - 3;
        return {
          type: "reference",
          name,
          range: [
            start,
            pos
          ],
          raw: str.substring(start, pos)
        };
      }
      function createGroup(behavior, disjunction, from, to) {
        return {
          type: "group",
          behavior,
          body: disjunction,
          range: [
            from,
            to
          ],
          raw: str.substring(from, to)
        };
      }
      function createQuantifier(min, max, from, to, symbol) {
        if (to == null) {
          from = pos - 1;
          to = pos;
        }
        return {
          type: "quantifier",
          min,
          max,
          greedy: true,
          body: null,
          // set later on
          symbol,
          range: [
            from,
            to
          ],
          raw: str.substring(from, to)
        };
      }
      function createAlternative(terms, from, to) {
        return {
          type: "alternative",
          body: terms,
          range: [
            from,
            to
          ],
          raw: str.substring(from, to)
        };
      }
      function createCharacterClass(contents, negative, from, to) {
        return {
          type: "characterClass",
          kind: contents.kind,
          body: contents.body,
          negative,
          range: [
            from,
            to
          ],
          raw: str.substring(from, to)
        };
      }
      function createClassRange(min, max, from, to) {
        if (min.codePoint > max.codePoint) {
          bail("invalid range in character class", min.raw + "-" + max.raw, from, to);
        }
        return {
          type: "characterClassRange",
          min,
          max,
          range: [
            from,
            to
          ],
          raw: str.substring(from, to)
        };
      }
      function createClassStrings(strings, from, to) {
        return {
          type: "classStrings",
          strings,
          range: [from, to],
          raw: str.substring(from, to)
        };
      }
      function createClassString(characters, from, to) {
        return {
          type: "classString",
          characters,
          range: [from, to],
          raw: str.substring(from, to)
        };
      }
      function flattenBody(body) {
        if (body.type === "alternative") {
          return body.body;
        } else {
          return [body];
        }
      }
      function incr(amount) {
        amount = amount || 1;
        pos += amount;
      }
      function consume(amount) {
        var res = str.substring(pos, pos += amount);
        return res;
      }
      function skip(value) {
        if (!match(value)) {
          bail("character", value);
        }
      }
      function match(value) {
        var len = value.length;
        if (str.substring(pos, pos + len) === value) {
          incr(len);
          return value;
        }
      }
      function matchOne(value) {
        if (str[pos] === value) {
          pos++;
          return value;
        }
      }
      function lookahead() {
        return str[pos];
      }
      function currentOne(value) {
        return str[pos] === value;
      }
      function current(value) {
        var len = value.length;
        return str.substring(pos, pos + len) === value;
      }
      function next(value) {
        return str[pos + 1] === value;
      }
      function matchReg(regExp) {
        var subStr = str.substring(pos);
        var res = subStr.match(regExp);
        if (res) {
          pos += res[0].length;
        }
        return res;
      }
      function parseDisjunction() {
        var res = [], from = pos;
        res.push(parseAlternative());
        while (matchOne("|")) {
          res.push(parseAlternative());
        }
        if (res.length === 1) {
          return res[0];
        }
        return createDisjunction(res, from, pos);
      }
      function parseAlternative() {
        var res = [], from = pos;
        var term;
        while (term = parseTerm()) {
          res.push(term);
        }
        if (res.length === 1) {
          return res[0];
        }
        return createAlternative(res, from, pos);
      }
      function parseTerm() {
        if (pos >= str.length || currentOne("|") || currentOne(")")) {
          return null;
        }
        var anchor = parseAnchor();
        var quantifier;
        if (anchor) {
          var pos_backup = pos;
          quantifier = parseQuantifier() || false;
          if (quantifier) {
            if (!isUnicodeMode && anchor.type === "group") {
              quantifier.body = flattenBody(anchor);
              updateRawStart(quantifier, anchor.range[0]);
              return quantifier;
            }
            pos = pos_backup;
            bail("Expected atom");
          }
          return anchor;
        }
        var atom = parseAtomAndExtendedAtom();
        if (!atom) {
          pos_backup = pos;
          quantifier = parseQuantifier() || false;
          if (quantifier) {
            pos = pos_backup;
            bail("Expected atom");
          }
          if (!isUnicodeMode && matchOne("{")) {
            atom = createCharacter("{");
          } else {
            bail("Expected atom");
          }
        }
        quantifier = parseQuantifier() || false;
        if (quantifier) {
          var type = atom.type, behavior = atom.behavior;
          if (type === "group" && (behavior === "negativeLookbehind" || behavior === "lookbehind")) {
            bail(
              "Invalid quantifier",
              "",
              quantifier.range[0],
              quantifier.range[1]
            );
          }
          quantifier.body = flattenBody(atom);
          updateRawStart(quantifier, atom.range[0]);
          return quantifier;
        }
        return atom;
      }
      function parseGroup(matchA, typeA, matchB, typeB) {
        var type = null, from = pos;
        if (match(matchA)) {
          type = typeA;
        } else if (match(matchB)) {
          type = typeB;
        } else {
          return false;
        }
        return finishGroup(type, from);
      }
      function finishGroup(type, from) {
        var body = parseDisjunction();
        if (!body) {
          bail("Expected disjunction");
        }
        skip(")");
        var group = createGroup(type, flattenBody(body), from, pos);
        if (type == "normal") {
          if (firstIteration) {
            closedCaptureCounter++;
          }
        }
        return group;
      }
      function parseAnchor() {
        switch (lookahead()) {
          case "^":
            incr();
            return createAnchor(
              "start",
              1
              /* rawLength */
            );
          case "$":
            incr();
            return createAnchor(
              "end",
              1
              /* rawLength */
            );
          case "\\": {
            if (next("b")) {
              incr(2);
              return createAnchor(
                "boundary",
                2
                /* rawLength */
              );
            } else if (next("B")) {
              incr(2);
              return createAnchor(
                "not-boundary",
                2
                /* rawLength */
              );
            }
            break;
          }
          case "(":
            return parseGroup("(?=", "lookahead", "(?!", "negativeLookahead");
          default:
            return;
        }
      }
      function parseQuantifier() {
        var res, from = pos;
        var quantifier;
        var min, max;
        switch (lookahead()) {
          case "*":
            incr();
            quantifier = createQuantifier(0, void 0, void 0, void 0, "*");
            break;
          case "+":
            incr();
            quantifier = createQuantifier(1, void 0, void 0, void 0, "+");
            break;
          case "?":
            incr();
            quantifier = createQuantifier(0, 1, void 0, void 0, "?");
            break;
          case "{": {
            if (res = matchReg(/^\{(\d+)\}/)) {
              min = parseInt(res[1], 10);
              quantifier = createQuantifier(min, min, from, pos);
            } else if (res = matchReg(/^\{(\d+),\}/)) {
              min = parseInt(res[1], 10);
              quantifier = createQuantifier(min, void 0, from, pos);
            } else if (res = matchReg(/^\{(\d+),(\d+)\}/)) {
              min = parseInt(res[1], 10);
              max = parseInt(res[2], 10);
              if (min > max) {
                bail("numbers out of order in {} quantifier", "", from, pos);
              }
              quantifier = createQuantifier(min, max, from, pos);
            }
            if (min && !Number.isSafeInteger(min) || max && !Number.isSafeInteger(max)) {
              bail("iterations outside JS safe integer range in quantifier", "", from, pos);
            }
          }
        }
        if (quantifier) {
          if (matchOne("?")) {
            quantifier.greedy = false;
            quantifier.range[1] += 1;
          }
        }
        return quantifier;
      }
      function parseAtomAndExtendedAtom() {
        var res;
        switch (res = lookahead()) {
          case ".":
            incr();
            return createDot();
          case "\\": {
            incr();
            res = parseAtomEscape();
            if (!res) {
              if (!isUnicodeMode && lookahead() == "c") {
                return createValue("symbol", 92, pos - 1, pos);
              }
              bail("atomEscape");
            }
            return res;
          }
          case "[":
            return parseCharacterClass();
          case "(": {
            if (features.lookbehind && (res = parseGroup("(?<=", "lookbehind", "(?<!", "negativeLookbehind"))) {
              return res;
            } else if (features.namedGroups && match("(?<")) {
              var name = parseIdentifier();
              skip(">");
              var group = finishGroup("normal", name.range[0] - 3);
              group.name = name;
              return group;
            } else if (features.modifiers && current("(?") && str[pos + 2] != ":") {
              return parseModifiersGroup();
            } else {
              return parseGroup("(?:", "ignore", "(", "normal");
            }
          }
          case "]":
          case "}":
            if (!isUnicodeMode) {
              incr();
              return createCharacter(res);
            }
            break;
          case "^":
          case "$":
          case "*":
          case "+":
          case "?":
          case "{":
          case ")":
          case "|":
            break;
          default:
            incr();
            return createCharacter(res);
        }
      }
      function parseModifiersGroup() {
        function hasDupChar(str2) {
          var i7 = 0;
          while (i7 < str2.length) {
            if (str2.indexOf(str2[i7], i7 + 1) != -1) {
              return true;
            }
            i7++;
          }
          return false;
        }
        var from = pos;
        incr(2);
        var enablingFlags = matchReg(/^[sim]+/);
        var disablingFlags;
        if (matchOne("-") && lookahead() !== ":") {
          disablingFlags = matchReg(/^[sim]+/);
          if (!disablingFlags) {
            bail("Invalid flags for modifiers group");
          }
        } else if (!enablingFlags) {
          bail("Invalid flags for modifiers group");
        }
        enablingFlags = enablingFlags ? enablingFlags[0] : "";
        disablingFlags = disablingFlags ? disablingFlags[0] : "";
        var flags2 = enablingFlags + disablingFlags;
        if (flags2.length > 3 || hasDupChar(flags2)) {
          bail("flags cannot be duplicated for modifiers group");
        }
        if (!matchOne(":")) {
          bail("Invalid flags for modifiers group");
        }
        var modifiersGroup = finishGroup("ignore", from);
        modifiersGroup.modifierFlags = {
          enabling: enablingFlags,
          disabling: disablingFlags
        };
        return modifiersGroup;
      }
      function parseUnicodeSurrogatePairEscape(firstEscape, isUnicodeMode2) {
        if (isUnicodeMode2) {
          var first, second;
          if (firstEscape.kind == "unicodeEscape" && (first = firstEscape.codePoint) >= 55296 && first <= 56319 && currentOne("\\") && next("u")) {
            var prevPos = pos;
            pos++;
            var secondEscape = parseClassEscape();
            if (secondEscape.kind == "unicodeEscape" && (second = secondEscape.codePoint) >= 56320 && second <= 57343) {
              firstEscape.kind = "unicodeCodePointEscape";
              firstEscape.codePoint = (first - 55296) * 1024 + second - 56320 + 65536;
              firstEscape.range[1] = pos;
              firstEscape.raw = str.substring(firstEscape.range[0], pos);
            } else {
              pos = prevPos;
            }
          }
        }
        return firstEscape;
      }
      function parseClassEscape() {
        return parseAtomEscape(true);
      }
      function parseAtomEscape(insideCharacterClass) {
        var res, from = pos, ch;
        switch (ch = lookahead()) {
          case "0":
          case "1":
          case "2":
          case "3":
          case "4":
          case "5":
          case "6":
          case "7":
          case "8":
          case "9":
            return parseDecimalEscape(insideCharacterClass);
          case "B": {
            if (insideCharacterClass) {
              bail("\\B not possible inside of CharacterClass", "", from);
              break;
            } else {
              return parseIdentityEscape();
            }
          }
          case "b": {
            if (insideCharacterClass) {
              incr();
              return createEscaped("singleEscape", 8, "\\b");
            } else {
              return parseIdentityEscape();
            }
          }
          case "c": {
            if (insideCharacterClass) {
              if (!isUnicodeMode && (res = matchReg(/^c(\d)/))) {
                return createEscaped("controlLetter", res[1] + 16, res[1], 2);
              } else if (!isUnicodeMode && match("c_")) {
                return createEscaped("controlLetter", 31, "_", 2);
              }
            }
            return parseCharacterEscape();
          }
          // CharacterClassEscape :: one of d D s S w W
          case "d":
          case "D":
          case "w":
          case "W":
          case "s":
          case "S":
            incr();
            return createCharacterClassEscape(ch);
          case "k":
            return parseNamedReference() || parseIdentityEscape();
          case "p":
          case "P":
            return parseUnicodePropertyEscape() || parseIdentityEscape();
          case "-": {
            if (insideCharacterClass && isUnicodeMode) {
              incr();
              return createEscaped("singleEscape", 45, "\\-");
            }
            return parseIdentityEscape();
          }
          default:
            return parseCharacterEscape();
        }
      }
      function parseDecimalEscape(insideCharacterClass) {
        var res, match2, from = pos;
        if (res = matchReg(/^(?!0)\d+/)) {
          match2 = res[0];
          var refIdx = parseInt(res[0], 10);
          if (refIdx <= closedCaptureCounter && !insideCharacterClass) {
            return createReference(res[0]);
          } else {
            backrefDenied.push(refIdx);
            if (firstIteration) {
              shouldReparse = true;
            } else {
              bailOctalEscapeIfUnicode(from, pos);
            }
            incr(-res[0].length);
            if (res = matchReg(/^[0-7]{1,3}/)) {
              return createEscaped("octal", parseInt(res[0], 8), res[0], 1);
            } else {
              var start = pos;
              res = createCharacter(matchReg(/^[89]/));
              return updateRawStart(res, start - 1);
            }
          }
        } else if (res = matchReg(/^[0-7]{1,3}/)) {
          match2 = res[0];
          if (match2 !== "0") {
            bailOctalEscapeIfUnicode(from, pos);
          }
          if (/^0{1,3}$/.test(match2)) {
            return createEscaped("null", 0, "0", match2.length);
          } else {
            return createEscaped("octal", parseInt(match2, 8), match2, 1);
          }
        }
        return false;
      }
      function bailOctalEscapeIfUnicode(from, pos2) {
        if (isUnicodeMode) {
          bail("Invalid decimal escape in unicode mode", null, from, pos2);
        }
      }
      function parseUnicodePropertyEscape() {
        var res, from = pos;
        if (features.unicodePropertyEscape && isUnicodeMode && (res = matchReg(/^([pP])\{([^}]+)\}/))) {
          return {
            type: "unicodePropertyEscape",
            negative: res[1] === "P",
            value: res[2],
            range: [from - 1, pos],
            raw: str.substring(from - 1, pos)
          };
        }
        return false;
      }
      function parseNamedReference() {
        if (features.namedGroups && matchReg(/^k<(?=.*?>)/)) {
          var name = parseIdentifier();
          skip(">");
          return createNamedReference(name);
        }
      }
      function parseRegExpUnicodeEscapeSequence(isUnicodeMode2) {
        var res;
        if (res = matchReg(/^u([0-9a-fA-F]{4})/)) {
          return parseUnicodeSurrogatePairEscape(
            createEscaped("unicodeEscape", parseInt(res[1], 16), res[1], 2),
            isUnicodeMode2
          );
        } else if (isUnicodeMode2 && (res = matchReg(/^u\{([0-9a-fA-F]+)\}/))) {
          return createEscaped("unicodeCodePointEscape", parseInt(res[1], 16), res[1], 4);
        }
      }
      function parseCharacterEscape() {
        var res;
        var from = pos;
        switch (lookahead()) {
          case "t":
            incr();
            return createEscaped("singleEscape", 9, "\\t");
          case "n":
            incr();
            return createEscaped("singleEscape", 10, "\\n");
          case "v":
            incr();
            return createEscaped("singleEscape", 11, "\\v");
          case "f":
            incr();
            return createEscaped("singleEscape", 12, "\\f");
          case "r":
            incr();
            return createEscaped("singleEscape", 13, "\\r");
          case "c":
            if (res = matchReg(/^c([a-zA-Z])/)) {
              return createEscaped("controlLetter", res[1].charCodeAt(0) % 32, res[1], 2);
            }
            break;
          case "x":
            if (res = matchReg(/^x([0-9a-fA-F]{2})/)) {
              return createEscaped("hexadecimalEscape", parseInt(res[1], 16), res[1], 2);
            }
            break;
          case "u":
            if (res = parseRegExpUnicodeEscapeSequence(isUnicodeMode)) {
              if (!res || res.codePoint > 1114111) {
                bail("Invalid escape sequence", null, from, pos);
              }
              return res;
            }
            break;
        }
        return parseIdentityEscape();
      }
      function parseIdentifierAtom(check) {
        var ch = lookahead();
        var from = pos;
        if (ch === "\\") {
          incr();
          var esc = parseRegExpUnicodeEscapeSequence(true);
          if (!esc || !check(esc.codePoint)) {
            bail("Invalid escape sequence", null, from, pos);
          }
          return fromCodePoint(esc.codePoint);
        }
        var code = ch.charCodeAt(0);
        if (code >= 55296 && code <= 56319) {
          ch += str[pos + 1];
          var second = ch.charCodeAt(1);
          if (second >= 56320 && second <= 57343) {
            code = (code - 55296) * 1024 + second - 56320 + 65536;
          }
        }
        if (!check(code)) return;
        incr();
        if (code > 65535) incr();
        return ch;
      }
      function parseIdentifier() {
        var start = pos;
        var res = parseIdentifierAtom(isIdentifierStart);
        if (!res) {
          bail("Invalid identifier");
        }
        var ch;
        while (ch = parseIdentifierAtom(isIdentifierPart)) {
          res += ch;
        }
        return {
          type: "identifier",
          value: res,
          range: [start, pos],
          raw: str.substring(start, pos)
        };
      }
      function isIdentifierStart(ch) {
        var NonAsciiIdentifierStart = /[\xAA\xB5\xBA\xC0-\xD6\xD8-\xF6\xF8-\u02C1\u02C6-\u02D1\u02E0-\u02E4\u02EC\u02EE\u0370-\u0374\u0376\u0377\u037A-\u037D\u037F\u0386\u0388-\u038A\u038C\u038E-\u03A1\u03A3-\u03F5\u03F7-\u0481\u048A-\u052F\u0531-\u0556\u0559\u0560-\u0588\u05D0-\u05EA\u05EF-\u05F2\u0620-\u064A\u066E\u066F\u0671-\u06D3\u06D5\u06E5\u06E6\u06EE\u06EF\u06FA-\u06FC\u06FF\u0710\u0712-\u072F\u074D-\u07A5\u07B1\u07CA-\u07EA\u07F4\u07F5\u07FA\u0800-\u0815\u081A\u0824\u0828\u0840-\u0858\u0860-\u086A\u0870-\u0887\u0889-\u088F\u08A0-\u08C9\u0904-\u0939\u093D\u0950\u0958-\u0961\u0971-\u0980\u0985-\u098C\u098F\u0990\u0993-\u09A8\u09AA-\u09B0\u09B2\u09B6-\u09B9\u09BD\u09CE\u09DC\u09DD\u09DF-\u09E1\u09F0\u09F1\u09FC\u0A05-\u0A0A\u0A0F\u0A10\u0A13-\u0A28\u0A2A-\u0A30\u0A32\u0A33\u0A35\u0A36\u0A38\u0A39\u0A59-\u0A5C\u0A5E\u0A72-\u0A74\u0A85-\u0A8D\u0A8F-\u0A91\u0A93-\u0AA8\u0AAA-\u0AB0\u0AB2\u0AB3\u0AB5-\u0AB9\u0ABD\u0AD0\u0AE0\u0AE1\u0AF9\u0B05-\u0B0C\u0B0F\u0B10\u0B13-\u0B28\u0B2A-\u0B30\u0B32\u0B33\u0B35-\u0B39\u0B3D\u0B5C\u0B5D\u0B5F-\u0B61\u0B71\u0B83\u0B85-\u0B8A\u0B8E-\u0B90\u0B92-\u0B95\u0B99\u0B9A\u0B9C\u0B9E\u0B9F\u0BA3\u0BA4\u0BA8-\u0BAA\u0BAE-\u0BB9\u0BD0\u0C05-\u0C0C\u0C0E-\u0C10\u0C12-\u0C28\u0C2A-\u0C39\u0C3D\u0C58-\u0C5A\u0C5C\u0C5D\u0C60\u0C61\u0C80\u0C85-\u0C8C\u0C8E-\u0C90\u0C92-\u0CA8\u0CAA-\u0CB3\u0CB5-\u0CB9\u0CBD\u0CDC-\u0CDE\u0CE0\u0CE1\u0CF1\u0CF2\u0D04-\u0D0C\u0D0E-\u0D10\u0D12-\u0D3A\u0D3D\u0D4E\u0D54-\u0D56\u0D5F-\u0D61\u0D7A-\u0D7F\u0D85-\u0D96\u0D9A-\u0DB1\u0DB3-\u0DBB\u0DBD\u0DC0-\u0DC6\u0E01-\u0E30\u0E32\u0E33\u0E40-\u0E46\u0E81\u0E82\u0E84\u0E86-\u0E8A\u0E8C-\u0EA3\u0EA5\u0EA7-\u0EB0\u0EB2\u0EB3\u0EBD\u0EC0-\u0EC4\u0EC6\u0EDC-\u0EDF\u0F00\u0F40-\u0F47\u0F49-\u0F6C\u0F88-\u0F8C\u1000-\u102A\u103F\u1050-\u1055\u105A-\u105D\u1061\u1065\u1066\u106E-\u1070\u1075-\u1081\u108E\u10A0-\u10C5\u10C7\u10CD\u10D0-\u10FA\u10FC-\u1248\u124A-\u124D\u1250-\u1256\u1258\u125A-\u125D\u1260-\u1288\u128A-\u128D\u1290-\u12B0\u12B2-\u12B5\u12B8-\u12BE\u12C0\u12C2-\u12C5\u12C8-\u12D6\u12D8-\u1310\u1312-\u1315\u1318-\u135A\u1380-\u138F\u13A0-\u13F5\u13F8-\u13FD\u1401-\u166C\u166F-\u167F\u1681-\u169A\u16A0-\u16EA\u16EE-\u16F8\u1700-\u1711\u171F-\u1731\u1740-\u1751\u1760-\u176C\u176E-\u1770\u1780-\u17B3\u17D7\u17DC\u1820-\u1878\u1880-\u18A8\u18AA\u18B0-\u18F5\u1900-\u191E\u1950-\u196D\u1970-\u1974\u1980-\u19AB\u19B0-\u19C9\u1A00-\u1A16\u1A20-\u1A54\u1AA7\u1B05-\u1B33\u1B45-\u1B4C\u1B83-\u1BA0\u1BAE\u1BAF\u1BBA-\u1BE5\u1C00-\u1C23\u1C4D-\u1C4F\u1C5A-\u1C7D\u1C80-\u1C8A\u1C90-\u1CBA\u1CBD-\u1CBF\u1CE9-\u1CEC\u1CEE-\u1CF3\u1CF5\u1CF6\u1CFA\u1D00-\u1DBF\u1E00-\u1F15\u1F18-\u1F1D\u1F20-\u1F45\u1F48-\u1F4D\u1F50-\u1F57\u1F59\u1F5B\u1F5D\u1F5F-\u1F7D\u1F80-\u1FB4\u1FB6-\u1FBC\u1FBE\u1FC2-\u1FC4\u1FC6-\u1FCC\u1FD0-\u1FD3\u1FD6-\u1FDB\u1FE0-\u1FEC\u1FF2-\u1FF4\u1FF6-\u1FFC\u2071\u207F\u2090-\u209C\u2102\u2107\u210A-\u2113\u2115\u2118-\u211D\u2124\u2126\u2128\u212A-\u2139\u213C-\u213F\u2145-\u2149\u214E\u2160-\u2188\u2C00-\u2CE4\u2CEB-\u2CEE\u2CF2\u2CF3\u2D00-\u2D25\u2D27\u2D2D\u2D30-\u2D67\u2D6F\u2D80-\u2D96\u2DA0-\u2DA6\u2DA8-\u2DAE\u2DB0-\u2DB6\u2DB8-\u2DBE\u2DC0-\u2DC6\u2DC8-\u2DCE\u2DD0-\u2DD6\u2DD8-\u2DDE\u3005-\u3007\u3021-\u3029\u3031-\u3035\u3038-\u303C\u3041-\u3096\u309B-\u309F\u30A1-\u30FA\u30FC-\u30FF\u3105-\u312F\u3131-\u318E\u31A0-\u31BF\u31F0-\u31FF\u3400-\u4DBF\u4E00-\uA48C\uA4D0-\uA4FD\uA500-\uA60C\uA610-\uA61F\uA62A\uA62B\uA640-\uA66E\uA67F-\uA69D\uA6A0-\uA6EF\uA717-\uA71F\uA722-\uA788\uA78B-\uA7DC\uA7F1-\uA801\uA803-\uA805\uA807-\uA80A\uA80C-\uA822\uA840-\uA873\uA882-\uA8B3\uA8F2-\uA8F7\uA8FB\uA8FD\uA8FE\uA90A-\uA925\uA930-\uA946\uA960-\uA97C\uA984-\uA9B2\uA9CF\uA9E0-\uA9E4\uA9E6-\uA9EF\uA9FA-\uA9FE\uAA00-\uAA28\uAA40-\uAA42\uAA44-\uAA4B\uAA60-\uAA76\uAA7A\uAA7E-\uAAAF\uAAB1\uAAB5\uAAB6\uAAB9-\uAABD\uAAC0\uAAC2\uAADB-\uAADD\uAAE0-\uAAEA\uAAF2-\uAAF4\uAB01-\uAB06\uAB09-\uAB0E\uAB11-\uAB16\uAB20-\uAB26\uAB28-\uAB2E\uAB30-\uAB5A\uAB5C-\uAB69\uAB70-\uABE2\uAC00-\uD7A3\uD7B0-\uD7C6\uD7CB-\uD7FB\uF900-\uFA6D\uFA70-\uFAD9\uFB00-\uFB06\uFB13-\uFB17\uFB1D\uFB1F-\uFB28\uFB2A-\uFB36\uFB38-\uFB3C\uFB3E\uFB40\uFB41\uFB43\uFB44\uFB46-\uFBB1\uFBD3-\uFD3D\uFD50-\uFD8F\uFD92-\uFDC7\uFDF0-\uFDFB\uFE70-\uFE74\uFE76-\uFEFC\uFF21-\uFF3A\uFF41-\uFF5A\uFF66-\uFFBE\uFFC2-\uFFC7\uFFCA-\uFFCF\uFFD2-\uFFD7\uFFDA-\uFFDC]|\uD800[\uDC00-\uDC0B\uDC0D-\uDC26\uDC28-\uDC3A\uDC3C\uDC3D\uDC3F-\uDC4D\uDC50-\uDC5D\uDC80-\uDCFA\uDD40-\uDD74\uDE80-\uDE9C\uDEA0-\uDED0\uDF00-\uDF1F\uDF2D-\uDF4A\uDF50-\uDF75\uDF80-\uDF9D\uDFA0-\uDFC3\uDFC8-\uDFCF\uDFD1-\uDFD5]|\uD801[\uDC00-\uDC9D\uDCB0-\uDCD3\uDCD8-\uDCFB\uDD00-\uDD27\uDD30-\uDD63\uDD70-\uDD7A\uDD7C-\uDD8A\uDD8C-\uDD92\uDD94\uDD95\uDD97-\uDDA1\uDDA3-\uDDB1\uDDB3-\uDDB9\uDDBB\uDDBC\uDDC0-\uDDF3\uDE00-\uDF36\uDF40-\uDF55\uDF60-\uDF67\uDF80-\uDF85\uDF87-\uDFB0\uDFB2-\uDFBA]|\uD802[\uDC00-\uDC05\uDC08\uDC0A-\uDC35\uDC37\uDC38\uDC3C\uDC3F-\uDC55\uDC60-\uDC76\uDC80-\uDC9E\uDCE0-\uDCF2\uDCF4\uDCF5\uDD00-\uDD15\uDD20-\uDD39\uDD40-\uDD59\uDD80-\uDDB7\uDDBE\uDDBF\uDE00\uDE10-\uDE13\uDE15-\uDE17\uDE19-\uDE35\uDE60-\uDE7C\uDE80-\uDE9C\uDEC0-\uDEC7\uDEC9-\uDEE4\uDF00-\uDF35\uDF40-\uDF55\uDF60-\uDF72\uDF80-\uDF91]|\uD803[\uDC00-\uDC48\uDC80-\uDCB2\uDCC0-\uDCF2\uDD00-\uDD23\uDD4A-\uDD65\uDD6F-\uDD85\uDE80-\uDEA9\uDEB0\uDEB1\uDEC2-\uDEC7\uDF00-\uDF1C\uDF27\uDF30-\uDF45\uDF70-\uDF81\uDFB0-\uDFC4\uDFE0-\uDFF6]|\uD804[\uDC03-\uDC37\uDC71\uDC72\uDC75\uDC83-\uDCAF\uDCD0-\uDCE8\uDD03-\uDD26\uDD44\uDD47\uDD50-\uDD72\uDD76\uDD83-\uDDB2\uDDC1-\uDDC4\uDDDA\uDDDC\uDE00-\uDE11\uDE13-\uDE2B\uDE3F\uDE40\uDE80-\uDE86\uDE88\uDE8A-\uDE8D\uDE8F-\uDE9D\uDE9F-\uDEA8\uDEB0-\uDEDE\uDF05-\uDF0C\uDF0F\uDF10\uDF13-\uDF28\uDF2A-\uDF30\uDF32\uDF33\uDF35-\uDF39\uDF3D\uDF50\uDF5D-\uDF61\uDF80-\uDF89\uDF8B\uDF8E\uDF90-\uDFB5\uDFB7\uDFD1\uDFD3]|\uD805[\uDC00-\uDC34\uDC47-\uDC4A\uDC5F-\uDC61\uDC80-\uDCAF\uDCC4\uDCC5\uDCC7\uDD80-\uDDAE\uDDD8-\uDDDB\uDE00-\uDE2F\uDE44\uDE80-\uDEAA\uDEB8\uDF00-\uDF1A\uDF40-\uDF46]|\uD806[\uDC00-\uDC2B\uDCA0-\uDCDF\uDCFF-\uDD06\uDD09\uDD0C-\uDD13\uDD15\uDD16\uDD18-\uDD2F\uDD3F\uDD41\uDDA0-\uDDA7\uDDAA-\uDDD0\uDDE1\uDDE3\uDE00\uDE0B-\uDE32\uDE3A\uDE50\uDE5C-\uDE89\uDE9D\uDEB0-\uDEF8\uDFC0-\uDFE0]|\uD807[\uDC00-\uDC08\uDC0A-\uDC2E\uDC40\uDC72-\uDC8F\uDD00-\uDD06\uDD08\uDD09\uDD0B-\uDD30\uDD46\uDD60-\uDD65\uDD67\uDD68\uDD6A-\uDD89\uDD98\uDDB0-\uDDDB\uDEE0-\uDEF2\uDF02\uDF04-\uDF10\uDF12-\uDF33\uDFB0]|\uD808[\uDC00-\uDF99]|\uD809[\uDC00-\uDC6E\uDC80-\uDD43]|\uD80B[\uDF90-\uDFF0]|[\uD80C\uD80E\uD80F\uD81C-\uD822\uD840-\uD868\uD86A-\uD86D\uD86F-\uD872\uD874-\uD879\uD880-\uD883\uD885-\uD88C][\uDC00-\uDFFF]|\uD80D[\uDC00-\uDC2F\uDC41-\uDC46\uDC60-\uDFFF]|\uD810[\uDC00-\uDFFA]|\uD811[\uDC00-\uDE46]|\uD818[\uDD00-\uDD1D]|\uD81A[\uDC00-\uDE38\uDE40-\uDE5E\uDE70-\uDEBE\uDED0-\uDEED\uDF00-\uDF2F\uDF40-\uDF43\uDF63-\uDF77\uDF7D-\uDF8F]|\uD81B[\uDD40-\uDD6C\uDE40-\uDE7F\uDEA0-\uDEB8\uDEBB-\uDED3\uDF00-\uDF4A\uDF50\uDF93-\uDF9F\uDFE0\uDFE1\uDFE3\uDFF2-\uDFF6]|\uD823[\uDC00-\uDCD5\uDCFF-\uDD1E\uDD80-\uDDF2]|\uD82B[\uDFF0-\uDFF3\uDFF5-\uDFFB\uDFFD\uDFFE]|\uD82C[\uDC00-\uDD22\uDD32\uDD50-\uDD52\uDD55\uDD64-\uDD67\uDD70-\uDEFB]|\uD82F[\uDC00-\uDC6A\uDC70-\uDC7C\uDC80-\uDC88\uDC90-\uDC99]|\uD835[\uDC00-\uDC54\uDC56-\uDC9C\uDC9E\uDC9F\uDCA2\uDCA5\uDCA6\uDCA9-\uDCAC\uDCAE-\uDCB9\uDCBB\uDCBD-\uDCC3\uDCC5-\uDD05\uDD07-\uDD0A\uDD0D-\uDD14\uDD16-\uDD1C\uDD1E-\uDD39\uDD3B-\uDD3E\uDD40-\uDD44\uDD46\uDD4A-\uDD50\uDD52-\uDEA5\uDEA8-\uDEC0\uDEC2-\uDEDA\uDEDC-\uDEFA\uDEFC-\uDF14\uDF16-\uDF34\uDF36-\uDF4E\uDF50-\uDF6E\uDF70-\uDF88\uDF8A-\uDFA8\uDFAA-\uDFC2\uDFC4-\uDFCB]|\uD837[\uDF00-\uDF1E\uDF25-\uDF2A]|\uD838[\uDC30-\uDC6D\uDD00-\uDD2C\uDD37-\uDD3D\uDD4E\uDE90-\uDEAD\uDEC0-\uDEEB]|\uD839[\uDCD0-\uDCEB\uDDD0-\uDDED\uDDF0\uDEC0-\uDEDE\uDEE0-\uDEE2\uDEE4\uDEE5\uDEE7-\uDEED\uDEF0-\uDEF4\uDEFE\uDEFF\uDFE0-\uDFE6\uDFE8-\uDFEB\uDFED\uDFEE\uDFF0-\uDFFE]|\uD83A[\uDC00-\uDCC4\uDD00-\uDD43\uDD4B]|\uD83B[\uDE00-\uDE03\uDE05-\uDE1F\uDE21\uDE22\uDE24\uDE27\uDE29-\uDE32\uDE34-\uDE37\uDE39\uDE3B\uDE42\uDE47\uDE49\uDE4B\uDE4D-\uDE4F\uDE51\uDE52\uDE54\uDE57\uDE59\uDE5B\uDE5D\uDE5F\uDE61\uDE62\uDE64\uDE67-\uDE6A\uDE6C-\uDE72\uDE74-\uDE77\uDE79-\uDE7C\uDE7E\uDE80-\uDE89\uDE8B-\uDE9B\uDEA1-\uDEA3\uDEA5-\uDEA9\uDEAB-\uDEBB]|\uD869[\uDC00-\uDEDF\uDF00-\uDFFF]|\uD86E[\uDC00-\uDC1D\uDC20-\uDFFF]|\uD873[\uDC00-\uDEAD\uDEB0-\uDFFF]|\uD87A[\uDC00-\uDFE0\uDFF0-\uDFFF]|\uD87B[\uDC00-\uDE5D]|\uD87E[\uDC00-\uDE1D]|\uD884[\uDC00-\uDF4A\uDF50-\uDFFF]|\uD88D[\uDC00-\uDC79]/;
        return ch === 36 || ch === 95 || // $ (dollar) and _ (underscore)
        ch >= 65 && ch <= 90 || // A..Z
        ch >= 97 && ch <= 122 || // a..z
        ch >= 128 && NonAsciiIdentifierStart.test(fromCodePoint(ch));
      }
      function isIdentifierPart(ch) {
        var NonAsciiIdentifierPartOnly = /[\xB7\u0300-\u036F\u0387\u0483-\u0487\u0591-\u05BD\u05BF\u05C1\u05C2\u05C4\u05C5\u05C7\u0610-\u061A\u064B-\u0669\u0670\u06D6-\u06DC\u06DF-\u06E4\u06E7\u06E8\u06EA-\u06ED\u06F0-\u06F9\u0711\u0730-\u074A\u07A6-\u07B0\u07C0-\u07C9\u07EB-\u07F3\u07FD\u0816-\u0819\u081B-\u0823\u0825-\u0827\u0829-\u082D\u0859-\u085B\u0897-\u089F\u08CA-\u08E1\u08E3-\u0903\u093A-\u093C\u093E-\u094F\u0951-\u0957\u0962\u0963\u0966-\u096F\u0981-\u0983\u09BC\u09BE-\u09C4\u09C7\u09C8\u09CB-\u09CD\u09D7\u09E2\u09E3\u09E6-\u09EF\u09FE\u0A01-\u0A03\u0A3C\u0A3E-\u0A42\u0A47\u0A48\u0A4B-\u0A4D\u0A51\u0A66-\u0A71\u0A75\u0A81-\u0A83\u0ABC\u0ABE-\u0AC5\u0AC7-\u0AC9\u0ACB-\u0ACD\u0AE2\u0AE3\u0AE6-\u0AEF\u0AFA-\u0AFF\u0B01-\u0B03\u0B3C\u0B3E-\u0B44\u0B47\u0B48\u0B4B-\u0B4D\u0B55-\u0B57\u0B62\u0B63\u0B66-\u0B6F\u0B82\u0BBE-\u0BC2\u0BC6-\u0BC8\u0BCA-\u0BCD\u0BD7\u0BE6-\u0BEF\u0C00-\u0C04\u0C3C\u0C3E-\u0C44\u0C46-\u0C48\u0C4A-\u0C4D\u0C55\u0C56\u0C62\u0C63\u0C66-\u0C6F\u0C81-\u0C83\u0CBC\u0CBE-\u0CC4\u0CC6-\u0CC8\u0CCA-\u0CCD\u0CD5\u0CD6\u0CE2\u0CE3\u0CE6-\u0CEF\u0CF3\u0D00-\u0D03\u0D3B\u0D3C\u0D3E-\u0D44\u0D46-\u0D48\u0D4A-\u0D4D\u0D57\u0D62\u0D63\u0D66-\u0D6F\u0D81-\u0D83\u0DCA\u0DCF-\u0DD4\u0DD6\u0DD8-\u0DDF\u0DE6-\u0DEF\u0DF2\u0DF3\u0E31\u0E34-\u0E3A\u0E47-\u0E4E\u0E50-\u0E59\u0EB1\u0EB4-\u0EBC\u0EC8-\u0ECE\u0ED0-\u0ED9\u0F18\u0F19\u0F20-\u0F29\u0F35\u0F37\u0F39\u0F3E\u0F3F\u0F71-\u0F84\u0F86\u0F87\u0F8D-\u0F97\u0F99-\u0FBC\u0FC6\u102B-\u103E\u1040-\u1049\u1056-\u1059\u105E-\u1060\u1062-\u1064\u1067-\u106D\u1071-\u1074\u1082-\u108D\u108F-\u109D\u135D-\u135F\u1369-\u1371\u1712-\u1715\u1732-\u1734\u1752\u1753\u1772\u1773\u17B4-\u17D3\u17DD\u17E0-\u17E9\u180B-\u180D\u180F-\u1819\u18A9\u1920-\u192B\u1930-\u193B\u1946-\u194F\u19D0-\u19DA\u1A17-\u1A1B\u1A55-\u1A5E\u1A60-\u1A7C\u1A7F-\u1A89\u1A90-\u1A99\u1AB0-\u1ABD\u1ABF-\u1ADD\u1AE0-\u1AEB\u1B00-\u1B04\u1B34-\u1B44\u1B50-\u1B59\u1B6B-\u1B73\u1B80-\u1B82\u1BA1-\u1BAD\u1BB0-\u1BB9\u1BE6-\u1BF3\u1C24-\u1C37\u1C40-\u1C49\u1C50-\u1C59\u1CD0-\u1CD2\u1CD4-\u1CE8\u1CED\u1CF4\u1CF7-\u1CF9\u1DC0-\u1DFF\u200C\u200D\u203F\u2040\u2054\u20D0-\u20DC\u20E1\u20E5-\u20F0\u2CEF-\u2CF1\u2D7F\u2DE0-\u2DFF\u302A-\u302F\u3099\u309A\u30FB\uA620-\uA629\uA66F\uA674-\uA67D\uA69E\uA69F\uA6F0\uA6F1\uA802\uA806\uA80B\uA823-\uA827\uA82C\uA880\uA881\uA8B4-\uA8C5\uA8D0-\uA8D9\uA8E0-\uA8F1\uA8FF-\uA909\uA926-\uA92D\uA947-\uA953\uA980-\uA983\uA9B3-\uA9C0\uA9D0-\uA9D9\uA9E5\uA9F0-\uA9F9\uAA29-\uAA36\uAA43\uAA4C\uAA4D\uAA50-\uAA59\uAA7B-\uAA7D\uAAB0\uAAB2-\uAAB4\uAAB7\uAAB8\uAABE\uAABF\uAAC1\uAAEB-\uAAEF\uAAF5\uAAF6\uABE3-\uABEA\uABEC\uABED\uABF0-\uABF9\uFB1E\uFE00-\uFE0F\uFE20-\uFE2F\uFE33\uFE34\uFE4D-\uFE4F\uFF10-\uFF19\uFF3F\uFF65]|\uD800[\uDDFD\uDEE0\uDF76-\uDF7A]|\uD801[\uDCA0-\uDCA9]|\uD802[\uDE01-\uDE03\uDE05\uDE06\uDE0C-\uDE0F\uDE38-\uDE3A\uDE3F\uDEE5\uDEE6]|\uD803[\uDD24-\uDD27\uDD30-\uDD39\uDD40-\uDD49\uDD69-\uDD6D\uDEAB\uDEAC\uDEFA-\uDEFF\uDF46-\uDF50\uDF82-\uDF85]|\uD804[\uDC00-\uDC02\uDC38-\uDC46\uDC66-\uDC70\uDC73\uDC74\uDC7F-\uDC82\uDCB0-\uDCBA\uDCC2\uDCF0-\uDCF9\uDD00-\uDD02\uDD27-\uDD34\uDD36-\uDD3F\uDD45\uDD46\uDD73\uDD80-\uDD82\uDDB3-\uDDC0\uDDC9-\uDDCC\uDDCE-\uDDD9\uDE2C-\uDE37\uDE3E\uDE41\uDEDF-\uDEEA\uDEF0-\uDEF9\uDF00-\uDF03\uDF3B\uDF3C\uDF3E-\uDF44\uDF47\uDF48\uDF4B-\uDF4D\uDF57\uDF62\uDF63\uDF66-\uDF6C\uDF70-\uDF74\uDFB8-\uDFC0\uDFC2\uDFC5\uDFC7-\uDFCA\uDFCC-\uDFD0\uDFD2\uDFE1\uDFE2]|\uD805[\uDC35-\uDC46\uDC50-\uDC59\uDC5E\uDCB0-\uDCC3\uDCD0-\uDCD9\uDDAF-\uDDB5\uDDB8-\uDDC0\uDDDC\uDDDD\uDE30-\uDE40\uDE50-\uDE59\uDEAB-\uDEB7\uDEC0-\uDEC9\uDED0-\uDEE3\uDF1D-\uDF2B\uDF30-\uDF39]|\uD806[\uDC2C-\uDC3A\uDCE0-\uDCE9\uDD30-\uDD35\uDD37\uDD38\uDD3B-\uDD3E\uDD40\uDD42\uDD43\uDD50-\uDD59\uDDD1-\uDDD7\uDDDA-\uDDE0\uDDE4\uDE01-\uDE0A\uDE33-\uDE39\uDE3B-\uDE3E\uDE47\uDE51-\uDE5B\uDE8A-\uDE99\uDF60-\uDF67\uDFF0-\uDFF9]|\uD807[\uDC2F-\uDC36\uDC38-\uDC3F\uDC50-\uDC59\uDC92-\uDCA7\uDCA9-\uDCB6\uDD31-\uDD36\uDD3A\uDD3C\uDD3D\uDD3F-\uDD45\uDD47\uDD50-\uDD59\uDD8A-\uDD8E\uDD90\uDD91\uDD93-\uDD97\uDDA0-\uDDA9\uDDE0-\uDDE9\uDEF3-\uDEF6\uDF00\uDF01\uDF03\uDF34-\uDF3A\uDF3E-\uDF42\uDF50-\uDF5A]|\uD80D[\uDC40\uDC47-\uDC55]|\uD818[\uDD1E-\uDD39]|\uD81A[\uDE60-\uDE69\uDEC0-\uDEC9\uDEF0-\uDEF4\uDF30-\uDF36\uDF50-\uDF59]|\uD81B[\uDD70-\uDD79\uDF4F\uDF51-\uDF87\uDF8F-\uDF92\uDFE4\uDFF0\uDFF1]|\uD82F[\uDC9D\uDC9E]|\uD833[\uDCF0-\uDCF9\uDF00-\uDF2D\uDF30-\uDF46]|\uD834[\uDD65-\uDD69\uDD6D-\uDD72\uDD7B-\uDD82\uDD85-\uDD8B\uDDAA-\uDDAD\uDE42-\uDE44]|\uD835[\uDFCE-\uDFFF]|\uD836[\uDE00-\uDE36\uDE3B-\uDE6C\uDE75\uDE84\uDE9B-\uDE9F\uDEA1-\uDEAF]|\uD838[\uDC00-\uDC06\uDC08-\uDC18\uDC1B-\uDC21\uDC23\uDC24\uDC26-\uDC2A\uDC8F\uDD30-\uDD36\uDD40-\uDD49\uDEAE\uDEEC-\uDEF9]|\uD839[\uDCEC-\uDCF9\uDDEE\uDDEF\uDDF1-\uDDFA\uDEE3\uDEE6\uDEEE\uDEEF\uDEF5]|\uD83A[\uDCD0-\uDCD6\uDD44-\uDD4A\uDD50-\uDD59]|\uD83E[\uDFF0-\uDFF9]|\uDB40[\uDD00-\uDDEF]/;
        return isIdentifierStart(ch) || ch >= 48 && ch <= 57 || // 0..9
        ch >= 128 && NonAsciiIdentifierPartOnly.test(fromCodePoint(ch));
      }
      function parseIdentityEscape() {
        var tmp;
        var l5 = lookahead();
        if (isUnicodeMode && /[\^$.*+?()\\[\]{}|/]/.test(l5) || !isUnicodeMode && l5 !== "c") {
          if (l5 === "k" && features.lookbehind) {
            return null;
          }
          tmp = consume(1);
          return createEscaped("identifier", tmp.charCodeAt(0), tmp, 1);
        }
        return null;
      }
      function parseCharacterClass() {
        var res, from = pos;
        if (res = match("[^")) {
          res = parseClassContents();
          skip("]");
          return createCharacterClass(res, true, from, pos);
        } else if (matchOne("[")) {
          res = parseClassContents();
          skip("]");
          return createCharacterClass(res, false, from, pos);
        }
        return null;
      }
      function parseClassContents() {
        var res;
        if (currentOne("]")) {
          return { kind: "union", body: [] };
        } else if (hasUnicodeSetFlag) {
          return parseClassSetExpression();
        } else {
          res = parseNonemptyClassRanges();
          if (!res) {
            bail("nonEmptyClassRanges");
          }
          return { kind: "union", body: res };
        }
      }
      function parseHelperClassContents(atom) {
        var from, to, res, atomTo, dash;
        if (currentOne("-") && !next("]")) {
          from = atom.range[0];
          incr();
          dash = createCharacter("-");
          atomTo = parseClassAtom();
          if (!atomTo) {
            bail("classAtom");
          }
          to = pos;
          var classContents = parseClassContents();
          if (!classContents) {
            bail("classContents");
          }
          if (!("codePoint" in atom) || !("codePoint" in atomTo)) {
            if (!isUnicodeMode) {
              res = [atom, dash, atomTo];
            } else {
              bail("invalid character class");
            }
          } else {
            res = [createClassRange(atom, atomTo, from, to)];
          }
          if (classContents.type === "empty") {
            return res;
          }
          return res.concat(classContents.body);
        }
        res = parseNonemptyClassRangesNoDash();
        if (!res) {
          bail("nonEmptyClassRangesNoDash");
        }
        return [atom].concat(res);
      }
      function parseNonemptyClassRanges() {
        var atom = parseClassAtom();
        if (!atom) {
          bail("classAtom");
        }
        if (currentOne("]")) {
          return [atom];
        }
        return parseHelperClassContents(atom);
      }
      function parseNonemptyClassRangesNoDash() {
        var res = parseClassAtom();
        if (!res) {
          bail("classAtom");
        }
        if (currentOne("]")) {
          return res;
        }
        return parseHelperClassContents(res);
      }
      function parseClassAtom() {
        if (matchOne("-")) {
          return createCharacter("-");
        } else {
          return parseClassAtomNoDash();
        }
      }
      function parseClassAtomNoDash() {
        var res;
        switch (res = lookahead()) {
          case "\\": {
            incr();
            res = parseClassEscape();
            if (!res) {
              if (!isUnicodeMode && lookahead() == "c") {
                return createCharacter("\\");
              }
              bail("classEscape");
            }
            return parseUnicodeSurrogatePairEscape(res, isUnicodeMode);
          }
          case "]":
          case "-":
            break;
          default:
            incr();
            return createCharacter(res);
        }
      }
      function parseClassSetExpression() {
        var body = [];
        var kind;
        var operand = parseClassSetOperand(
          /* allowRanges*/
          true
        );
        body.push(operand);
        if (operand.type === "classRange") {
          kind = "union";
        } else if (currentOne("&")) {
          kind = "intersection";
        } else if (currentOne("-")) {
          kind = "subtraction";
        } else {
          kind = "union";
        }
        while (!currentOne("]")) {
          if (kind === "intersection") {
            skip("&");
            skip("&");
            if (currentOne("&")) {
              bail("&& cannot be followed by &. Wrap it in brackets: &&[&].");
            }
          } else if (kind === "subtraction") {
            skip("-");
            skip("-");
          }
          operand = parseClassSetOperand(
            /* allowRanges*/
            kind === "union"
          );
          body.push(operand);
        }
        return { kind, body };
      }
      function parseClassSetOperand(allowRanges) {
        var from = pos;
        var start, res;
        if (matchOne("\\")) {
          if (match("q{")) {
            return parseClassStringDisjunction();
          } else if (res = parseClassEscape()) {
            start = res;
          } else if (res = parseClassSetCharacterEscapedHelper()) {
            return res;
          } else {
            bail("Invalid escape", "\\" + lookahead(), from);
          }
        } else if (res = parseClassSetCharacterUnescapedHelper()) {
          start = res;
        } else if (res = parseCharacterClass()) {
          return res;
        } else {
          bail("Invalid character", lookahead());
        }
        if (allowRanges && currentOne("-") && !next("-")) {
          incr();
          if (res = parseClassSetCharacter()) {
            return createClassRange(start, res, from, pos);
          }
          bail("Invalid range end", lookahead());
        }
        return start;
      }
      function parseClassSetCharacter() {
        if (matchOne("\\")) {
          var res, from = pos;
          if (res = parseClassSetCharacterEscapedHelper()) {
            return res;
          } else {
            bail("Invalid escape", "\\" + lookahead(), from);
          }
        }
        return parseClassSetCharacterUnescapedHelper();
      }
      function parseClassSetCharacterUnescapedHelper() {
        var res;
        if (matchReg(/^(?:&&|!!|##|\$\$|%%|\*\*|\+\+|,,|\.\.|::|;;|<<|==|>>|\?\?|@@|\^\^|``|~~)/)) {
          bail("Invalid set operation in character class");
        }
        if (res = matchReg(/^[^()[\]{}/\-\\|]/)) {
          return createCharacter(res);
        }
      }
      function parseClassSetCharacterEscapedHelper() {
        var res;
        if (matchOne("b")) {
          return createEscaped("singleEscape", 8, "\\b");
        } else if (matchOne("B")) {
          bail("\\B not possible inside of ClassContents", "", pos - 2);
        } else if (res = matchReg(/^[&\-!#%,:;<=>@`~]/)) {
          return createEscaped("identifier", res[0].codePointAt(0), res[0]);
        } else if (res = parseCharacterEscape()) {
          return res;
        } else {
          return null;
        }
      }
      function parseClassStringDisjunction() {
        var from = pos - 3;
        var res = [];
        do {
          res.push(parseClassString());
        } while (matchOne("|"));
        skip("}");
        return createClassStrings(res, from, pos);
      }
      function parseClassString() {
        var res = [], from = pos;
        var char;
        while (char = parseClassSetCharacter()) {
          res.push(char);
        }
        return createClassString(res, from, pos);
      }
      function bail(message, details, from, to) {
        from = from == null ? pos : from;
        to = to == null ? from : to;
        var contextStart = Math.max(0, from - 10);
        var contextEnd = Math.min(to + 10, str.length);
        var context = "    " + str.substring(contextStart, contextEnd);
        var pointer = "    " + new Array(from - contextStart + 1).join(" ") + "^";
        throw SyntaxError(message + " at position " + from + (details ? ": " + details : "") + "\n" + context + "\n" + pointer);
      }
      var backrefDenied = [];
      var closedCaptureCounter = 0;
      var firstIteration = true;
      var shouldReparse = false;
      var hasUnicodeFlag = (flags || "").indexOf("u") !== -1;
      var hasUnicodeSetFlag = (flags || "").indexOf("v") !== -1;
      var isUnicodeMode = hasUnicodeFlag || hasUnicodeSetFlag;
      var pos = 0;
      if (hasUnicodeSetFlag && !features.unicodeSet) {
        throw new Error('The "v" flag is only supported when the .unicodeSet option is enabled.');
      }
      if (hasUnicodeFlag && hasUnicodeSetFlag) {
        throw new Error('The "u" and "v" flags are mutually exclusive.');
      }
      str = String(str);
      if (str === "") {
        str = "(?:)";
      }
      var result = parseDisjunction();
      if (result.range[1] !== str.length) {
        bail("Could not parse entire input - got stuck", "", result.range[1]);
      }
      shouldReparse = shouldReparse || backrefDenied.some(function(ref) {
        return ref <= closedCaptureCounter;
      });
      if (shouldReparse) {
        pos = 0;
        firstIteration = false;
        return parseDisjunction();
      }
      return result;
    }
    var regjsparser = {
      parse: parse2
    };
    if (module.exports) {
      module.exports = regjsparser;
    } else {
      window.regjsparser = regjsparser;
    }
  })();
})(parser);
var parserExports = parser.exports;
function parse(pattern, unicode) {
  var ast = parserExports.parse(pattern, unicode ? "u" : "", {
    lookbehind: true,
    unicodePropertyEscape: true
  });
  return ast;
}
function last(input) {
  var length = input.length;
  return length ? input[length - 1] : null;
}
function dropCommon(a4, b4) {
  var commonLevels;
  for (commonLevels = 0; commonLevels < a4.length && commonLevels < b4.length && a4[commonLevels] === b4[commonLevels]; commonLevels++)
    ;
  return {
    a: a4.slice(commonLevels),
    b: b4.slice(commonLevels)
  };
}
var lookaheadBehaviours$1 = [
  "lookahead",
  "lookbehind",
  "negativeLookahead",
  "negativeLookbehind"
];
function replace(input, replacement, start, end) {
  return "".concat(input.slice(0, start)).concat(replacement).concat(input.slice(end));
}
function shiftOffsets(_a) {
  var after = _a.after, offsets = _a.offsets, shiftAmount = _a.shiftAmount;
  return new Set(__spreadArray([], __read(offsets), false).map(function(offset) {
    return offset > after ? offset + shiftAmount : offset;
  }));
}
function quantifierIterationsToString(quantifier) {
  if (quantifier.symbol) {
    return "".concat(quantifier.symbol).concat(quantifier.greedy ? "" : "?");
  }
  if (quantifier.min === quantifier.max) {
    return "{".concat(quantifier.min, "}");
  }
  return "{".concat(quantifier.min, ",").concat(quantifier.max || "", "}").concat(quantifier.greedy ? "" : "?");
}
function alreadyHasStartAnchorReplacement(node) {
  if (node.type !== "alternative")
    return false;
  var body = node.body;
  if (body.length < 1)
    return false;
  var _a = __read(body, 1), maybeQuantifier = _a[0];
  if (maybeQuantifier.type !== "quantifier")
    return false;
  if (maybeQuantifier.min !== 0)
    return false;
  if (maybeQuantifier.max !== void 0)
    return false;
  if (maybeQuantifier.greedy)
    return false;
  var maybeCharacterClass = maybeQuantifier.body[0];
  if (maybeCharacterClass.type !== "characterClass")
    return false;
  maybeCharacterClass.kind;
  if (maybeCharacterClass.body.length !== 0)
    return false;
  if (!maybeCharacterClass.negative)
    return false;
  return true;
}
function isMissingStartAnchor(rootNode) {
  if (alreadyHasStartAnchorReplacement(rootNode))
    return false;
  var check = function(node) {
    var e_1, _a;
    if (node.type === "anchor") {
      if (node.kind === "start")
        return "anchor";
      return null;
    }
    if (node.type === "quantifier" && node.max === 0)
      return null;
    if (node.type === "alternative" || node.type === "group" || node.type === "quantifier") {
      var mayBeSkipped = node.type === "quantifier" && node.min === 0;
      try {
        for (var _b = __values(node.body), _c = _b.next(); !_c.done; _c = _b.next()) {
          var child = _c.value;
          var res = check(child);
          if (res === "consumingNode")
            return "consumingNode";
          if (res === "anchor") {
            if (!mayBeSkipped)
              return "anchor";
            return null;
          }
        }
      } catch (e_1_1) {
        e_1 = { error: e_1_1 };
      } finally {
        try {
          if (_c && !_c.done && (_a = _b.return)) _a.call(_b);
        } finally {
          if (e_1) throw e_1.error;
        }
      }
      return null;
    }
    if (node.type === "disjunction") {
      var res = node.body.map(function(child2) {
        return check(child2);
      });
      if (res.some(function(a4) {
        return a4 === "consumingNode";
      }))
        return "consumingNode";
      if (res.every(function(a4) {
        return a4 === "anchor";
      }))
        return "anchor";
      return null;
    }
    return "consumingNode";
  };
  return check(rootNode) === "consumingNode";
}
function getRawWithoutCapturingGroupsOrLookaheads(rootNode) {
  var referencesWithOffset = /* @__PURE__ */ new Map();
  var walk = function(node, offset) {
    var walkAll = function(nodes, startOffset) {
      var result2 = "";
      nodes.forEach(function(a4) {
        result2 += walk(a4, startOffset + result2.length);
      });
      return result2;
    };
    switch (node.type) {
      case "anchor":
      case "characterClass":
      case "characterClassEscape":
      case "unicodePropertyEscape":
      case "value":
      case "dot":
        return node.raw;
      case "reference": {
        referencesWithOffset.set(node, offset);
        return node.raw;
      }
      case "group": {
        switch (node.behavior) {
          case "normal":
            return "(?:".concat(walkAll(node.body, offset + 3), ")");
          case "ignore":
            return "(?:".concat(walkAll(node.body, offset + 3), ")");
          case "lookahead":
          case "lookbehind":
          case "negativeLookahead":
          case "negativeLookbehind":
            return "";
        }
      }
      // eslint-disable-next-line no-fallthrough
      case "disjunction": {
        var res_1 = "";
        node.body.forEach(function(a4, i7) {
          if (i7 > 0)
            res_1 += "|";
          res_1 += walk(a4, offset + res_1.length);
        });
        return res_1;
      }
      case "alternative":
        return walkAll(node.body, offset);
      case "quantifier": {
        return "".concat(walk(node.body[0], offset)).concat(quantifierIterationsToString(node));
      }
    }
  };
  var result = walk(rootNode, 0);
  return {
    referencesWithOffset,
    result
  };
}
function downgradePattern(_a) {
  var pattern = _a.pattern, unicode = _a.unicode;
  var run = function(lastResult2) {
    var ast = parse(lastResult2.pattern, unicode);
    var actions = [];
    var needsStartAnchorReplacement = false;
    var needsWrappingInGroup = ast.type === "disjunction";
    var groups = /* @__PURE__ */ new Map();
    var infiniteGroups = /* @__PURE__ */ new Set();
    var nextGroupIndex = 1;
    var lookaheadOnlyContainsGroup = function(lookahead, group) {
      if (lookahead.body.length !== 1)
        return false;
      var node = lookahead.body[0];
      return node === group;
    };
    var walkAll = function(_a2) {
      var nodeStack = _a2.nodeStack, nodes = _a2.nodes, passedStartAnchor = _a2.passedStartAnchor, serial = _a2.serial;
      var justHadLookahead = null;
      nodes.forEach(function(expression) {
        if (serial) {
          walk({
            immediatelyPrecedingLookahead: justHadLookahead,
            node: expression,
            nodeStack,
            passedStartAnchor
          });
          justHadLookahead = expression.type === "group" && expression.behavior === "lookahead" ? expression : null;
        } else {
          walk({
            immediatelyPrecedingLookahead: justHadLookahead,
            node: expression,
            nodeStack,
            passedStartAnchor: __assign({}, passedStartAnchor)
          });
        }
      });
    };
    var walk = function(_a2) {
      var immediatelyPrecedingLookahead = _a2.immediatelyPrecedingLookahead, node = _a2.node, nodeStack = _a2.nodeStack, passedStartAnchor = _a2.passedStartAnchor;
      var onConsumingNode = function() {
        if (!passedStartAnchor.value) {
          needsStartAnchorReplacement = true;
        }
      };
      switch (node.type) {
        case "characterClass":
        case "characterClassEscape":
        case "unicodePropertyEscape":
        case "value":
        case "dot":
          onConsumingNode();
          return;
        case "anchor": {
          if (node.kind === "start") {
            passedStartAnchor.value = true;
          }
          return;
        }
        case "group": {
          var groupIndex = null;
          if (node.behavior === "normal") {
            groupIndex = nextGroupIndex;
            nextGroupIndex++;
          }
          walkAll({
            nodeStack: __spreadArray(__spreadArray([], __read(nodeStack), false), [node], false),
            nodes: node.body,
            passedStartAnchor,
            serial: true
          });
          if (groupIndex !== null && node.behavior === "normal") {
            groups.set(groupIndex, { group: node, stack: __spreadArray([], __read(nodeStack), false) });
          }
          return;
        }
        case "disjunction": {
          walkAll({
            nodeStack: __spreadArray(__spreadArray([], __read(nodeStack), false), [node], false),
            nodes: node.body,
            passedStartAnchor,
            serial: false
          });
          return;
        }
        case "alternative": {
          walkAll({
            nodeStack: __spreadArray(__spreadArray([], __read(nodeStack), false), [node], false),
            nodes: node.body,
            passedStartAnchor,
            serial: true
          });
          return;
        }
        case "quantifier": {
          if (node.max !== 0) {
            if (node.max === void 0) {
              __spreadArray([], __read(nodeStack), false).reverse().some(function(stackNode) {
                if (stackNode.type === "group") {
                  if (lookaheadBehaviours$1.indexOf(stackNode.behavior) >= 0) {
                    return true;
                  }
                  infiniteGroups.add(stackNode);
                }
                return false;
              });
            }
            walkAll({
              nodeStack: __spreadArray(__spreadArray([], __read(nodeStack), false), [node], false),
              nodes: node.body,
              passedStartAnchor: node.min === 0 ? __assign({}, passedStartAnchor) : passedStartAnchor,
              serial: true
            });
          }
          return;
        }
        case "reference": {
          onConsumingNode();
          var entry = groups.get(node.matchIndex);
          if (entry) {
            var group = entry.group, stack = entry.stack;
            var localStack = dropCommon(stack, nodeStack).a;
            var lookaheadStack = localStack.flatMap(function(stackNode) {
              return stackNode.type === "group" && lookaheadBehaviours$1.indexOf(stackNode.behavior) >= 0 ? [stackNode] : [];
            });
            var groupMayNotBeReached = localStack.some(function(stackNode) {
              return stackNode.type === "disjunction" || stackNode.type === "quantifier" && stackNode.min === 0;
            });
            var groupInLookahead = lookaheadStack.length > 0;
            var groupCouldBeSet = lookaheadStack.every(function(value) {
              return value.behavior !== "negativeLookahead" && value.behavior !== "negativeLookbehind";
            });
            if (groupCouldBeSet && (groupInLookahead || infiniteGroups.has(group))) {
              var atomic = !!immediatelyPrecedingLookahead && lookaheadOnlyContainsGroup(immediatelyPrecedingLookahead, group);
              var optional = groupInLookahead && groupMayNotBeReached;
              actions.push({
                atomicOrOptional: atomic ? "atomic" : optional ? "optional" : null,
                group,
                reference: node
              });
            }
          }
          return;
        }
      }
    };
    walk({
      immediatelyPrecedingLookahead: null,
      node: ast,
      nodeStack: [],
      passedStartAnchor: { value: false }
    });
    var newPattern = lastResult2.pattern;
    var atomicGroupOffsets = new Set(lastResult2.atomicGroupOffsets);
    var needToRerun2 = false;
    __spreadArray([], __read(actions), false).sort(function(a4, b4) {
      return b4.reference.range[0] - a4.reference.range[0];
    }).forEach(function(action) {
      var atomicOrOptional = action.atomicOrOptional, group = action.group, _a2 = __read(action.reference.range, 2), referenceStart = _a2[0], referenceEnd = _a2[1];
      var _b2 = getRawWithoutCapturingGroupsOrLookaheads(group), result2 = _b2.result, referencesWithOffset = _b2.referencesWithOffset;
      if (referencesWithOffset.size > 0) {
        needToRerun2 = true;
      }
      var replacement = atomicOrOptional === "optional" ? "(?:".concat(result2, "?)") : result2;
      newPattern = replace(newPattern, replacement, referenceStart, referenceEnd);
      var shiftAmount = replacement.length - (referenceEnd - referenceStart);
      atomicGroupOffsets = shiftOffsets({
        after: referenceStart,
        offsets: atomicGroupOffsets,
        shiftAmount
      });
      if (atomicOrOptional === "atomic") {
        atomicGroupOffsets.add(referenceStart);
      }
      actions.forEach(function(_a3) {
        var innerAtomicOrOptional = _a3.atomicOrOptional, innerReference = _a3.reference;
        if (innerAtomicOrOptional === "atomic") {
          var offset = referencesWithOffset.get(innerReference);
          if (offset !== void 0) {
            atomicGroupOffsets.add(referenceStart + offset);
          }
        }
      });
    });
    if (needsStartAnchorReplacement && !alreadyHasStartAnchorReplacement(ast)) {
      if (needsWrappingInGroup) {
        newPattern = "(?:".concat(newPattern, ")");
      }
      newPattern = "[^]*?".concat(newPattern);
      atomicGroupOffsets = shiftOffsets({
        after: -1,
        offsets: atomicGroupOffsets,
        shiftAmount: needsWrappingInGroup ? 8 : 5
      });
    }
    return { needToRerun: needToRerun2, result: { atomicGroupOffsets, pattern: newPattern } };
  };
  var lastResult = {
    atomicGroupOffsets: /* @__PURE__ */ new Set(),
    pattern
  };
  for (; ; ) {
    var _b = run(lastResult), result = _b.result, needToRerun = _b.needToRerun;
    lastResult = result;
    if (!needToRerun) {
      break;
    }
  }
  return lastResult;
}
function buildFinalizationRegistry(cleanupCallback) {
  return new FinalizationRegistry(cleanupCallback);
}
var forkSymbol = /* @__PURE__ */ Symbol("fork");
var pendingResultsSymbol = /* @__PURE__ */ Symbol("pendingResults");
function buildForkableIterator(source) {
  var onResult = /* @__PURE__ */ new Set();
  var registry = buildFinalizationRegistry(function(onResultCallback) {
    onResult.delete(onResultCallback);
  });
  var readSource = function() {
    var result = source.next();
    onResult.forEach(function(fn) {
      return fn(result);
    });
  };
  var makeFork = function(initialPendingResults) {
    var _a;
    var iterator = (_a = {}, _a[forkSymbol] = function() {
      return makeFork(this[pendingResultsSymbol]);
    }, _a[pendingResultsSymbol] = initialPendingResults.slice(0), _a.next = function(value) {
      if (value !== void 0) {
        throw new Error("`ForkableIterator` `next()` cannot take a value");
      }
      var pendingResults = this[pendingResultsSymbol];
      if (!pendingResults.length) {
        readSource();
      }
      return pendingResults.shift();
    }, _a);
    var ref = new WeakRef(iterator);
    var callback = function(result) {
      var maybeIterator = ref.deref();
      maybeIterator === null || maybeIterator === void 0 ? void 0 : maybeIterator[pendingResultsSymbol].push(result);
    };
    registry.register(iterator, callback);
    onResult.add(callback);
    return iterator;
  };
  return makeFork([]);
}
function fork(forkableIterator) {
  if (!forkableIterator || !forkableIterator[forkSymbol]) {
    throw new Error("The provided value was not a `ForkableIterator` from `buildForkableIterator()`");
  }
  return forkableIterator[forkSymbol]();
}
function emptyReader() {
  return __generator(this, function(_a) {
    return [
      2
      /*return*/
    ];
  });
}
function buildForkableReader(sourceReader) {
  return buildForkableIterator(sourceReader);
}
function chainReaders(readers) {
  var readers_1, readers_1_1, reader, next, e_1_1;
  var e_1, _a;
  return __generator(this, function(_b) {
    switch (_b.label) {
      case 0:
        _b.trys.push([0, 6, 7, 8]);
        readers_1 = __values(readers), readers_1_1 = readers_1.next();
        _b.label = 1;
      case 1:
        if (!!readers_1_1.done) return [3, 5];
        reader = readers_1_1.value;
        next = void 0;
        _b.label = 2;
      case 2:
        if (!!(next = reader.next()).done) return [3, 4];
        return [4, next.value];
      case 3:
        _b.sent();
        return [3, 2];
      case 4:
        readers_1_1 = readers_1.next();
        return [3, 1];
      case 5:
        return [3, 8];
      case 6:
        e_1_1 = _b.sent();
        e_1 = { error: e_1_1 };
        return [3, 8];
      case 7:
        try {
          if (readers_1_1 && !readers_1_1.done && (_a = readers_1.return)) _a.call(readers_1);
        } finally {
          if (e_1) throw e_1.error;
        }
        return [
          7
          /*endfinally*/
        ];
      case 8:
        return [
          2
          /*return*/
        ];
    }
  });
}
function buildArrayReader(input) {
  return __generator(this, function(_a) {
    switch (_a.label) {
      case 0:
        return [5, __values(input)];
      case 1:
        _a.sent();
        return [
          2
          /*return*/
        ];
    }
  });
}
function subtractRanges(source, toSubtract) {
  var res = [];
  if (source[0] < toSubtract[0]) {
    res.push([source[0], Math.min(toSubtract[0] - 1, source[1])]);
  }
  if (source[1] > toSubtract[1]) {
    res.push([Math.max(toSubtract[1] + 1, source[0]), source[1]]);
  }
  return res;
}
function intersectRanges(a4, b4) {
  var startShared = Math.max(a4[0], b4[0]);
  var endShared = Math.min(a4[1], b4[1]);
  if (startShared > endShared) {
    return null;
  }
  return [startShared, endShared];
}
function createRanges(set) {
  var ascending = __spreadArray([], __read(set), false).sort(function(a4, b4) {
    return a4 - b4;
  });
  var ranges = [];
  var startIndex = 0;
  for (var i7 = 0; i7 < ascending.length; i7++) {
    var startValue = ascending[startIndex];
    var currentValue = ascending[i7];
    var nextValue = i7 + 1 < ascending.length ? ascending[i7 + 1] : null;
    if (nextValue === null || nextValue - startValue !== i7 - startIndex + 1) {
      ranges.push([startValue, currentValue]);
      startIndex = i7 + 1;
    }
  }
  return ranges;
}
function invertRanges(ranges) {
  var result = [];
  for (var i7 = 0; i7 < ranges.length + 1; i7++) {
    var prev = i7 - 1 >= 0 ? ranges[i7 - 1] : null;
    var current = i7 < ranges.length ? ranges[i7] : null;
    var start = prev ? prev[1] + 1 : -Infinity;
    var end = current ? current[0] - 1 : Infinity;
    if (start - end === 1)
      continue;
    if (start > end)
      throw new Error("Internal error: invalid ranges input");
    result.push([start, end]);
  }
  return result;
}
function isEmptyCharacterGroups(group) {
  return !group.rangesNegated && !group.ranges.length && !group.unicodePropertyEscapes.size;
}
function intersectCharacterGroups(a4, b4) {
  var e_1, _a;
  var _b, _c;
  var newRanges;
  var newNegated;
  if (!a4.rangesNegated) {
    if (!b4.rangesNegated) {
      newNegated = false;
      newRanges = [];
      a4.ranges.forEach(function(aRange) {
        b4.ranges.forEach(function(bRange) {
          var intersection = intersectRanges(aRange, bRange);
          if (intersection)
            newRanges.push(intersection);
        });
      });
    } else {
      newNegated = false;
      newRanges = __spreadArray([], __read(a4.ranges), false);
      b4.ranges.forEach(function(bRange) {
        var narrowed = [];
        newRanges.forEach(function(aRange) {
          narrowed.push.apply(narrowed, __spreadArray([], __read(subtractRanges(aRange, bRange)), false));
        });
        newRanges = narrowed;
      });
    }
  } else {
    if (!b4.rangesNegated) {
      newNegated = false;
      newRanges = __spreadArray([], __read(b4.ranges), false);
      a4.ranges.forEach(function(aRange) {
        var narrowed = [];
        newRanges.forEach(function(bRange) {
          narrowed.push.apply(narrowed, __spreadArray([], __read(subtractRanges(bRange, aRange)), false));
        });
        newRanges = narrowed;
      });
    } else {
      newNegated = true;
      newRanges = __spreadArray(__spreadArray([], __read(a4.ranges), false), __read(b4.ranges), false);
    }
  }
  var allUnicodePropertyEscapes = new Set(__spreadArray(__spreadArray([], __read(a4.unicodePropertyEscapes.keys()), false), __read(b4.unicodePropertyEscapes.keys()), false));
  var newUnicodePropertyEscapes = /* @__PURE__ */ new Map();
  try {
    for (var allUnicodePropertyEscapes_1 = __values(allUnicodePropertyEscapes), allUnicodePropertyEscapes_1_1 = allUnicodePropertyEscapes_1.next(); !allUnicodePropertyEscapes_1_1.done; allUnicodePropertyEscapes_1_1 = allUnicodePropertyEscapes_1.next()) {
      var unicodePropertyEscape = allUnicodePropertyEscapes_1_1.value;
      var aEscape = (_b = a4.unicodePropertyEscapes.get(unicodePropertyEscape)) !== null && _b !== void 0 ? _b : null;
      var bEscape = (_c = b4.unicodePropertyEscapes.get(unicodePropertyEscape)) !== null && _c !== void 0 ? _c : null;
      if (aEscape !== null && bEscape === null) {
        newUnicodePropertyEscapes.set(unicodePropertyEscape, aEscape);
      } else if (aEscape === null && bEscape !== null) {
        newUnicodePropertyEscapes.set(unicodePropertyEscape, bEscape);
      } else if (aEscape !== null && bEscape !== null && aEscape === bEscape) {
        newUnicodePropertyEscapes.set(unicodePropertyEscape, aEscape);
      }
    }
  } catch (e_1_1) {
    e_1 = { error: e_1_1 };
  } finally {
    try {
      if (allUnicodePropertyEscapes_1_1 && !allUnicodePropertyEscapes_1_1.done && (_a = allUnicodePropertyEscapes_1.return)) _a.call(allUnicodePropertyEscapes_1);
    } finally {
      if (e_1) throw e_1.error;
    }
  }
  return {
    ranges: newRanges,
    rangesNegated: newNegated,
    unicodePropertyEscapes: newUnicodePropertyEscapes
  };
}
function buildEndReader(offset) {
  return __generator(this, function(_a) {
    switch (_a.label) {
      case 0:
        return [4, {
          bounded: false,
          offset,
          stack: [],
          subType: "end",
          type: characterReaderTypeCharacterEntry
        }];
      case 1:
        _a.sent();
        throw new Error("Internal error: should not be reading after end");
    }
  });
}
function join(getAction, getReader) {
  var _join = function(_getAction, _getReader, timeSinceEmitSomething) {
    var i7, action, emittedSomething, reader, next, _loop_1;
    if (timeSinceEmitSomething === void 0) {
      timeSinceEmitSomething = 0;
    }
    return __generator(this, function(_a) {
      switch (_a.label) {
        case 0:
          i7 = 0;
          _a.label = 1;
        case 1:
          action = _getAction(i7, timeSinceEmitSomething);
          if (!(action === "stop")) return [3, 2];
          return [
            2
            /*return*/
          ];
        case 2:
          if (!(action === "fork")) return [3, 4];
          return [4, {
            reader: function() {
              return emptyReader();
            },
            subType: null,
            type: characterReaderTypeSplit
          }];
        case 3:
          _a.sent();
          _a.label = 4;
        case 4:
          emittedSomething = false;
          reader = _getReader(i7);
          next = void 0;
          _loop_1 = function() {
            var value, _b, _i_1, _timeSinceEmittedSomething_1;
            return __generator(this, function(_c) {
              switch (_c.label) {
                case 0:
                  value = next.value;
                  _b = value.type;
                  switch (_b) {
                    case characterReaderTypeSplit:
                      return [3, 1];
                    case characterReaderTypeCharacterEntry:
                      return [3, 3];
                  }
                  return [3, 5];
                case 1:
                  _i_1 = i7;
                  _timeSinceEmittedSomething_1 = timeSinceEmitSomething;
                  return [4, {
                    reader: function() {
                      return _join(function(innerIndex, innerTimeSinceEmittedSomething) {
                        return innerIndex === 0 ? "continue" : _getAction(innerIndex + _i_1, innerTimeSinceEmittedSomething);
                      }, function(innerIndex) {
                        return innerIndex === 0 ? value.reader() : _getReader(innerIndex + _i_1);
                      }, _timeSinceEmittedSomething_1);
                    },
                    subType: value.subType,
                    type: characterReaderTypeSplit
                  }];
                case 2:
                  _c.sent();
                  return [3, 5];
                case 3:
                  if (value.subType !== "null" && value.subType !== "start" && value.subType !== "end") {
                    emittedSomething = true;
                  }
                  return [4, value];
                case 4:
                  _c.sent();
                  return [3, 5];
                case 5:
                  return [
                    2
                    /*return*/
                  ];
              }
            });
          };
          _a.label = 5;
        case 5:
          if (!!(next = reader.next()).done) return [3, 7];
          return [5, _loop_1()];
        case 6:
          _a.sent();
          return [3, 5];
        case 7:
          timeSinceEmitSomething = emittedSomething ? 0 : timeSinceEmitSomething + 1;
          _a.label = 8;
        case 8:
          i7++;
          return [3, 1];
        case 9:
          return [
            2
            /*return*/
          ];
      }
    });
  };
  return _join(getAction, getReader);
}
function joinArray(input) {
  var length = input.length;
  return join(function(i7) {
    return i7 < length ? "continue" : "stop";
  }, function(i7) {
    return input[i7]();
  });
}
function buildSequenceCharacterReader(_a) {
  var caseInsensitive = _a.caseInsensitive, dotAll = _a.dotAll, nodes = _a.nodes;
  return joinArray(nodes.map(function(node) {
    return function() {
      return buildCharacterReader({ caseInsensitive, dotAll, node });
    };
  }));
}
function map(reader, handle) {
  var startThread = function(innerReader) {
    var next, _loop_1;
    return __generator(this, function(_a) {
      switch (_a.label) {
        case 0:
          _loop_1 = function() {
            var value, _b;
            return __generator(this, function(_c) {
              switch (_c.label) {
                case 0:
                  value = next.value;
                  _b = value.type;
                  switch (_b) {
                    case characterReaderTypeSplit:
                      return [3, 1];
                    case characterReaderTypeCharacterEntry:
                      return [3, 3];
                  }
                  return [3, 5];
                case 1:
                  return [4, {
                    reader: function() {
                      return startThread(value.reader());
                    },
                    subType: value.subType,
                    type: characterReaderTypeSplit
                  }];
                case 2:
                  _c.sent();
                  return [3, 5];
                case 3:
                  return [4, handle(value)];
                case 4:
                  _c.sent();
                  return [3, 5];
                case 5:
                  return [
                    2
                    /*return*/
                  ];
              }
            });
          };
          _a.label = 1;
        case 1:
          if (!!(next = innerReader.next()).done) return [3, 3];
          return [5, _loop_1()];
        case 2:
          _a.sent();
          return [3, 1];
        case 3:
          return [
            2
            /*return*/
          ];
      }
    });
  };
  return startThread(reader);
}
function getGroups(stack) {
  return new Set(stack.flatMap(function(entry) {
    return entry.type === "group" ? [entry.group] : [];
  }));
}
function getLookaheadStack(stack) {
  var e_1, _a;
  var lookaheadStack = [];
  try {
    for (var stack_1 = __values(stack), stack_1_1 = stack_1.next(); !stack_1_1.done; stack_1_1 = stack_1.next()) {
      var entry = stack_1_1.value;
      if (entry.type === "group") {
        var group = entry.group;
        var behavior = group.behavior;
        if (behavior === "lookbehind" || behavior === "negativeLookbehind" || behavior === "negativeLookahead" || behavior === "lookahead") {
          lookaheadStack.push(group);
        }
      }
    }
  } catch (e_1_1) {
    e_1 = { error: e_1_1 };
  } finally {
    try {
      if (stack_1_1 && !stack_1_1.done && (_a = stack_1.return)) _a.call(stack_1);
    } finally {
      if (e_1) throw e_1.error;
    }
  }
  return lookaheadStack;
}
function buildGroupCharacterReader(_a) {
  var caseInsensitive = _a.caseInsensitive, dotAll = _a.dotAll, node = _a.node;
  switch (node.behavior) {
    case "lookbehind":
    case "negativeLookbehind":
    case "lookahead":
    case "negativeLookahead": {
      return buildArrayReader([
        {
          reader: function() {
            return joinArray([
              function() {
                return map(buildSequenceCharacterReader({
                  caseInsensitive,
                  dotAll,
                  nodes: node.body
                }), function(value) {
                  return __assign(__assign({}, value), { stack: __spreadArray([{ group: node, type: "group" }], __read(value.stack), false) });
                });
              },
              function() {
                return buildEndReader(node.range[1]);
              }
            ]);
          },
          subType: node.behavior,
          type: characterReaderTypeSplit
        }
      ]);
    }
    case "ignore":
    case "normal": {
      return map(buildSequenceCharacterReader({
        caseInsensitive,
        dotAll,
        nodes: node.body
      }), function(value) {
        return __assign(__assign({}, value), { stack: __spreadArray([{ group: node, type: "group" }], __read(value.stack), false) });
      });
    }
  }
}
function buildNullCharacterReader(offset) {
  return __generator(this, function(_a) {
    switch (_a.label) {
      case 0:
        return [4, {
          offset,
          stack: [],
          subType: "null",
          type: characterReaderTypeCharacterEntry
        }];
      case 1:
        _a.sent();
        return [
          2
          /*return*/
        ];
    }
  });
}
function mustGet(map2, key) {
  if (!map2.has(key)) {
    throw new Error("Internal error: map missing key");
  }
  return map2.get(key);
}
function buildQuantifiersInInfinitePortion(stack, nodeExtra) {
  var exhaustive = stack.some(function(entry) {
    if (entry.type !== "group" || entry.group.behavior !== "normal") {
      return false;
    }
    var index = mustGet(nodeExtra.capturingGroupToIndex, entry.group);
    var hasReference = nodeExtra.reachableReferences.some(function(reference) {
      return reference.matchIndex === index;
    });
    return hasReference;
  });
  if (exhaustive)
    return /* @__PURE__ */ new Set();
  return new Set(stack.flatMap(function(entry) {
    return entry.type === "quantifier" ? [entry] : [];
  }).filter(function(_a) {
    var iteration = _a.iteration, quantifier = _a.quantifier;
    return iteration >= 1 && iteration >= quantifier.min;
  }).map(function(_a) {
    var quantifier = _a.quantifier;
    return quantifier;
  }));
}
function buildQuantifierIterations(stack) {
  var res = /* @__PURE__ */ new Map();
  stack.flatMap(function(entry) {
    return entry.type === "quantifier" ? [entry] : [];
  }).forEach(function(_a) {
    var iteration = _a.iteration, quantifier = _a.quantifier;
    return res.set(quantifier, iteration);
  });
  return res;
}
function buildQuantifierCharacterReader(_a) {
  var caseInsensitive = _a.caseInsensitive, dotAll = _a.dotAll, node = _a.node;
  var min = node.min, _b = node.max, max = _b === void 0 ? Infinity : _b;
  return joinArray([
    // always emit the null first so that we always emit something in cases where the loop is empty
    function() {
      return buildNullCharacterReader(node.body[0].range[0]);
    },
    function() {
      return join(function(i7, timeSinceEmit) {
        if (timeSinceEmit > 1)
          return "stop";
        if (i7 >= max)
          return "stop";
        if (i7 >= min)
          return "fork";
        return "continue";
      }, function(i7) {
        return map(joinArray(__spreadArray(__spreadArray([], __read(i7 > 0 ? [
          function() {
            return buildNullCharacterReader(node.body[0].range[0]);
          }
        ] : []), false), [
          function() {
            return buildCharacterReader({
              caseInsensitive,
              dotAll,
              node: node.body[0]
            });
          }
        ], false)), function(value) {
          return __assign(__assign({}, value), { stack: __spreadArray([
            {
              iteration: i7,
              quantifier: node,
              type: "quantifier"
            }
          ], __read(value.stack), false) });
        });
      });
    }
  ]);
}
function buildAnchorReader(node) {
  var _a;
  return __generator(this, function(_b) {
    switch (_b.label) {
      case 0:
        _a = node.kind;
        switch (_a) {
          case "end":
            return [3, 1];
          case "start":
            return [3, 3];
          case "boundary":
            return [3, 5];
          case "not-boundary":
            return [3, 5];
        }
        return [3, 6];
      case 1:
        return [4, {
          bounded: true,
          offset: node.range[0],
          stack: [],
          subType: "end",
          type: characterReaderTypeCharacterEntry
        }];
      case 2:
        _b.sent();
        throw new Error("Internal error: should not be reading after end");
      case 3:
        return [4, {
          offset: node.range[0],
          stack: [],
          subType: "start",
          type: characterReaderTypeCharacterEntry
        }];
      case 4:
        _b.sent();
        return [3, 6];
      case 5: {
        return [3, 6];
      }
      case 6:
        return [
          2
          /*return*/
        ];
    }
  });
}
var dRanges = [[48, 57]];
var inverseDRanges = invertRanges(dRanges);
var wRanges = [
  [48, 57],
  [65, 90],
  [95, 95],
  [97, 122]
];
var inverseWRanges = invertRanges(wRanges);
var sRanges = [
  [9, 9],
  [10, 10],
  [11, 11],
  [12, 12],
  [13, 13],
  [32, 32],
  [160, 160],
  [5760, 5760],
  [8192, 8202],
  [8232, 8233],
  [8239, 8239],
  [8287, 8287],
  [12288, 12288],
  [65279, 65279]
];
var inverseSRanges = invertRanges(sRanges);
function characterClassEscapeToRange(value) {
  switch (value) {
    case "d":
    case "D": {
      return value === "d" ? dRanges : inverseDRanges;
    }
    case "w":
    case "W": {
      return value === "w" ? wRanges : inverseWRanges;
    }
    case "s":
    case "S": {
      return value === "s" ? sRanges : inverseSRanges;
    }
  }
}
function buildCharacterClassEscapeReader(node) {
  var value, ranges;
  return __generator(this, function(_a) {
    switch (_a.label) {
      case 0:
        value = node.value;
        ranges = characterClassEscapeToRange(value);
        return [4, {
          characterGroups: {
            ranges,
            rangesNegated: false,
            unicodePropertyEscapes: /* @__PURE__ */ new Map()
          },
          node,
          stack: [],
          subType: "groups",
          type: characterReaderTypeCharacterEntry
        }];
      case 1:
        _a.sent();
        return [
          2
          /*return*/
        ];
    }
  });
}
function toCodePoint(input) {
  if (input.length > 1)
    return null;
  var codePoint = input.codePointAt(0);
  if (!codePoint)
    throw new Error("Internal error: expected codepoint");
  return codePoint;
}
function toUpperCaseCodePoint(codePoint) {
  var upperCase = String.fromCodePoint(codePoint).toUpperCase();
  var upperCodePoint = toCodePoint(upperCase);
  return upperCodePoint !== null ? upperCodePoint : codePoint;
}
function buildCodePointRanges(_a) {
  var caseInsensitive = _a.caseInsensitive, highCodePoint = _a.highCodePoint, lowCodePoint = _a.lowCodePoint;
  if (!caseInsensitive) {
    return [[lowCodePoint, highCodePoint]];
  }
  var codePoints = /* @__PURE__ */ new Set();
  for (var codePoint = lowCodePoint; codePoint <= highCodePoint; codePoint++) {
    codePoints.add(toUpperCaseCodePoint(codePoint));
  }
  return createRanges(codePoints);
}
function codePointFromValue(_a) {
  var caseInsensitive = _a.caseInsensitive, value = _a.value;
  var codePoint = value.codePoint;
  return caseInsensitive ? toUpperCaseCodePoint(codePoint) : codePoint;
}
function buildValueCharacterReader(_a) {
  var caseInsensitive = _a.caseInsensitive, node = _a.node;
  var codePoint = codePointFromValue({ caseInsensitive, value: node });
  return buildArrayReader([
    {
      characterGroups: {
        ranges: [[codePoint, codePoint]],
        rangesNegated: false,
        unicodePropertyEscapes: /* @__PURE__ */ new Map()
      },
      node,
      stack: [],
      subType: "groups",
      type: characterReaderTypeCharacterEntry
    }
  ]);
}
function buildCharacterClassCharacterReader(_a) {
  var e_1, _b;
  var caseInsensitive = _a.caseInsensitive, node = _a.node;
  var ranges = [];
  var unicodePropertyEscapes = /* @__PURE__ */ new Map();
  var matchesEverything = false;
  try {
    outer: for (var _c = __values(node.body), _d = _c.next(); !_d.done; _d = _c.next()) {
      var expression = _d.value;
      switch (expression.type) {
        case "value": {
          var codePoint = codePointFromValue({
            caseInsensitive,
            value: expression
          });
          ranges.push([codePoint, codePoint]);
          break;
        }
        case "characterClassRange": {
          ranges.push.apply(ranges, __spreadArray([], __read(buildCodePointRanges({
            caseInsensitive,
            highCodePoint: expression.max.codePoint,
            lowCodePoint: expression.min.codePoint
          })), false));
          break;
        }
        case "characterClassEscape": {
          var value = expression.value;
          ranges.push.apply(ranges, __spreadArray([], __read(characterClassEscapeToRange(value)), false));
          break;
        }
        case "unicodePropertyEscape": {
          var resolvedNegative = expression.negative !== node.negative;
          if (unicodePropertyEscapes.get(expression.value) === !resolvedNegative) {
            matchesEverything = true;
            break outer;
          }
          unicodePropertyEscapes.set(expression.value, resolvedNegative);
          break;
        }
      }
    }
  } catch (e_1_1) {
    e_1 = { error: e_1_1 };
  } finally {
    try {
      if (_d && !_d.done && (_b = _c.return)) _b.call(_c);
    } finally {
      if (e_1) throw e_1.error;
    }
  }
  return buildArrayReader([
    {
      characterGroups: matchesEverything ? { ranges: [], rangesNegated: true, unicodePropertyEscapes: /* @__PURE__ */ new Map() } : {
        ranges,
        rangesNegated: node.negative,
        unicodePropertyEscapes
      },
      node,
      stack: [],
      subType: "groups",
      type: characterReaderTypeCharacterEntry
    }
  ]);
}
function buildDisjunctionCharacterReader(_a) {
  var caseInsensitive = _a.caseInsensitive, dotAll = _a.dotAll, node = _a.node;
  return chainReaders([
    buildArrayReader(node.body.slice(0, -1).map(function(part) {
      return {
        reader: function() {
          return buildCharacterReader({ caseInsensitive, dotAll, node: part });
        },
        subType: null,
        type: characterReaderTypeSplit
      };
    })),
    buildCharacterReader({
      caseInsensitive,
      dotAll,
      node: node.body[node.body.length - 1]
    })
  ]);
}
function buildDotCharacterReader(_a) {
  var dotAll = _a.dotAll, node = _a.node;
  return __generator(this, function(_b) {
    switch (_b.label) {
      case 0:
        return [4, {
          characterGroups: {
            ranges: dotAll ? [] : (
              // [\n\r\u2028-\u2029]
              [
                [10, 10],
                [13, 13],
                [8232, 8233]
              ]
            ),
            rangesNegated: true,
            unicodePropertyEscapes: /* @__PURE__ */ new Map()
          },
          node,
          stack: [],
          subType: "groups",
          type: characterReaderTypeCharacterEntry
        }];
      case 1:
        _b.sent();
        return [
          2
          /*return*/
        ];
    }
  });
}
function buildReferenceCharacterReader(node) {
  return __generator(this, function(_a) {
    switch (_a.label) {
      case 0:
        return [4, {
          node,
          referenceIndex: node.matchIndex,
          stack: [],
          subType: "reference",
          type: characterReaderTypeCharacterEntry
        }];
      case 1:
        _a.sent();
        return [
          2
          /*return*/
        ];
    }
  });
}
function buildUnicodePropertyEscapeCharacterReader(node) {
  return buildArrayReader([
    {
      characterGroups: {
        ranges: [],
        rangesNegated: false,
        unicodePropertyEscapes: /* @__PURE__ */ new Map([[node.value, node.negative]])
      },
      node,
      stack: [],
      subType: "groups",
      type: characterReaderTypeCharacterEntry
    }
  ]);
}
var characterReaderTypeCharacterEntry = /* @__PURE__ */ Symbol("characterReaderTypeCharacterEntry");
var characterReaderTypeSplit = /* @__PURE__ */ Symbol("characterReaderTypeSplit");
function buildCharacterReader(_a) {
  var caseInsensitive = _a.caseInsensitive, dotAll = _a.dotAll, node = _a.node;
  switch (node.type) {
    case "anchor":
      return buildAnchorReader(node);
    case "characterClass":
      return buildCharacterClassCharacterReader({ caseInsensitive, node });
    case "characterClassEscape":
      return buildCharacterClassEscapeReader(node);
    case "unicodePropertyEscape":
      return buildUnicodePropertyEscapeCharacterReader(node);
    case "reference":
      return buildReferenceCharacterReader(node);
    case "value":
      return buildValueCharacterReader({ caseInsensitive, node });
    case "dot":
      return buildDotCharacterReader({ dotAll, node });
    case "alternative":
      return buildSequenceCharacterReader({
        caseInsensitive,
        dotAll,
        nodes: node.body
      });
    case "disjunction":
      return buildDisjunctionCharacterReader({ caseInsensitive, dotAll, node });
    case "group":
      return buildGroupCharacterReader({ caseInsensitive, dotAll, node });
    case "quantifier":
      return buildQuantifierCharacterReader({ caseInsensitive, dotAll, node });
  }
}
var characterReaderLevel1TypeSplit = /* @__PURE__ */ Symbol("characterReaderLevel2TypeSplit");
var characterReaderLevel1TypeEntry = /* @__PURE__ */ Symbol("characterReaderLevel1TypeEntry");
function buildCharacterReaderLevel1(_a) {
  var caseInsensitive = _a.caseInsensitive, dotAll = _a.dotAll, node = _a.node;
  var startThread = function(reader, precedingZeroWidthEntries) {
    var next, _loop_1, state_1;
    return __generator(this, function(_a2) {
      switch (_a2.label) {
        case 0:
          _loop_1 = function() {
            var _b, _c, value_1, _precedingZeroWidthEntries_1;
            return __generator(this, function(_d) {
              switch (_d.label) {
                case 0:
                  _b = next.value.type;
                  switch (_b) {
                    case characterReaderTypeCharacterEntry:
                      return [3, 1];
                    case characterReaderTypeSplit:
                      return [3, 10];
                  }
                  return [3, 12];
                case 1:
                  _c = next.value.subType;
                  switch (_c) {
                    case "groups":
                      return [3, 2];
                    case "reference":
                      return [3, 4];
                    case "end":
                      return [3, 6];
                    case "null":
                      return [3, 7];
                    case "start":
                      return [3, 8];
                  }
                  return [3, 9];
                case 2:
                  return [4, {
                    characterGroups: next.value.characterGroups,
                    node: next.value.node,
                    precedingZeroWidthEntries,
                    stack: next.value.stack,
                    subType: "groups",
                    type: characterReaderLevel1TypeEntry
                  }];
                case 3:
                  _d.sent();
                  precedingZeroWidthEntries = [];
                  return [3, 9];
                case 4:
                  return [4, {
                    node: next.value.node,
                    precedingZeroWidthEntries,
                    referenceIndex: next.value.referenceIndex,
                    stack: next.value.stack,
                    subType: next.value.subType,
                    type: characterReaderLevel1TypeEntry
                  }];
                case 5:
                  _d.sent();
                  precedingZeroWidthEntries = [];
                  return [3, 9];
                case 6: {
                  return [2, { value: {
                    bounded: next.value.bounded,
                    precedingZeroWidthEntries
                  } }];
                }
                case 7: {
                  precedingZeroWidthEntries = __spreadArray(__spreadArray([], __read(precedingZeroWidthEntries), false), [
                    {
                      offset: next.value.offset,
                      stack: next.value.stack,
                      type: "null"
                    }
                  ], false);
                  return [3, 9];
                }
                case 8: {
                  precedingZeroWidthEntries = __spreadArray(__spreadArray([], __read(precedingZeroWidthEntries), false), [
                    {
                      offset: next.value.offset,
                      stack: next.value.stack,
                      type: "start"
                    }
                  ], false);
                  return [3, 9];
                }
                case 9:
                  return [3, 12];
                case 10:
                  value_1 = next.value;
                  _precedingZeroWidthEntries_1 = precedingZeroWidthEntries;
                  return [4, {
                    reader: function() {
                      return startThread(value_1.reader(), _precedingZeroWidthEntries_1);
                    },
                    subType: value_1.subType,
                    type: characterReaderLevel1TypeSplit
                  }];
                case 11:
                  _d.sent();
                  return [3, 12];
                case 12:
                  return [
                    2
                    /*return*/
                  ];
              }
            });
          };
          _a2.label = 1;
        case 1:
          if (!!(next = reader.next()).done) return [3, 3];
          return [5, _loop_1()];
        case 2:
          state_1 = _a2.sent();
          if (typeof state_1 === "object")
            return [2, state_1.value];
          return [3, 1];
        case 3:
          return [2, { bounded: false, precedingZeroWidthEntries }];
      }
    });
  };
  return startThread(buildCharacterReader({ caseInsensitive, dotAll, node }), []);
}
var characterReaderLevel2TypeSplit = /* @__PURE__ */ Symbol("characterReaderLevel2TypeSplit");
var characterReaderLevel2TypeEntry = /* @__PURE__ */ Symbol("characterReaderLevel2TypeEntry");
var internalReaderTypeSplit = /* @__PURE__ */ Symbol("internalReaderTypeSplit");
var internalReaderTypeEntry = /* @__PURE__ */ Symbol("internalReaderTypeEntry");
function characterReaderLevel1ToInternalReader(characterReaderLevel1) {
  var next, _loop_1;
  return __generator(this, function(_a) {
    switch (_a.label) {
      case 0:
        _loop_1 = function() {
          var value, _b;
          return __generator(this, function(_c) {
            switch (_c.label) {
              case 0:
                value = next.value;
                _b = value.type;
                switch (_b) {
                  case characterReaderLevel1TypeSplit:
                    return [3, 1];
                  case characterReaderLevel1TypeEntry:
                    return [3, 3];
                }
                return [3, 5];
              case 1:
                return [4, {
                  reader: function() {
                    return characterReaderLevel1ToInternalReader(value.reader());
                  },
                  subType: value.subType,
                  type: internalReaderTypeSplit
                }];
              case 2:
                _c.sent();
                return [3, 5];
              case 3:
                return [4, __assign(__assign({}, value), { type: internalReaderTypeEntry })];
              case 4:
                _c.sent();
                return [3, 5];
              case 5:
                return [
                  2
                  /*return*/
                ];
            }
          });
        };
        _a.label = 1;
      case 1:
        if (!!(next = characterReaderLevel1.next()).done) return [3, 3];
        return [5, _loop_1()];
      case 2:
        _a.sent();
        return [3, 1];
      case 3:
        return [2, next.value];
    }
  });
}
function haveHadCompleteIteration(before, now) {
  var e_1, _a;
  try {
    for (var now_1 = __values(now), now_1_1 = now_1.next(); !now_1_1.done; now_1_1 = now_1.next()) {
      var _b = __read(now_1_1.value, 2), quantifier = _b[0], iterationsNow = _b[1];
      var iterationsBefore = before.get(quantifier) || 0;
      if (iterationsNow - iterationsBefore > 1) {
        return true;
      }
    }
  } catch (e_1_1) {
    e_1 = { error: e_1_1 };
  } finally {
    try {
      if (now_1_1 && !now_1_1.done && (_a = now_1.return)) _a.call(now_1);
    } finally {
      if (e_1) throw e_1.error;
    }
  }
  return false;
}
function getGroupContentsReader(_a) {
  var group, groupContents, groupLookaheadStack, _b, _c, groupEntry, e_2_1;
  var e_2, _d;
  var groupContentsStore = _a.groupContentsStore, groups = _a.groups, groupsWithInfiniteSize = _a.groupsWithInfiniteSize, nodeExtra = _a.nodeExtra, value = _a.value;
  return __generator(this, function(_e) {
    switch (_e.label) {
      case 0:
        group = mustGet(nodeExtra.indexToCapturingGroup, value.referenceIndex);
        groupContents = groupContentsStore.get(value.referenceIndex) || {
          contents: [],
          group
        };
        groupLookaheadStack = dropCommon(mustGet(nodeExtra.nodeToLookaheadStack, groupContents.group), mustGet(nodeExtra.nodeToLookaheadStack, value.node)).a;
        if (groupLookaheadStack.length) {
          if (groupLookaheadStack.some(function(_a2) {
            var behavior = _a2.behavior;
            return behavior === "negativeLookahead" || behavior === "negativeLookbehind";
          })) {
            return [2, { bounded: false, precedingZeroWidthEntries: [] }];
          }
          throw new Error("Unsupported reference (".concat(value.referenceIndex, " at position ").concat(value.node.range[0], "). Pattern needs downgrading. See the `downgradePattern` option."));
        }
        if (groups.has(groupContents.group)) {
          return [2, { bounded: false, precedingZeroWidthEntries: [] }];
        }
        if (groupsWithInfiniteSize.has(value.referenceIndex)) {
          throw new Error("Unsupported reference to group ".concat(value.referenceIndex, " as group is not a finite size. Pattern needs downgrading. See the `downgradePattern` option."));
        }
        _e.label = 1;
      case 1:
        _e.trys.push([1, 6, 7, 8]);
        _b = __values(groupContents.contents), _c = _b.next();
        _e.label = 2;
      case 2:
        if (!!_c.done) return [3, 5];
        groupEntry = _c.value;
        return [4, {
          characterGroups: groupEntry.characterGroups,
          node: groupEntry.node,
          precedingZeroWidthEntries: groupEntry.precedingZeroWidthEntries,
          stack: __spreadArray(__spreadArray(__spreadArray([], __read(groupEntry.stack), false), [
            {
              reference: value.node,
              type: "reference"
            }
          ], false), __read(value.stack), false),
          subType: "groups",
          type: internalReaderTypeEntry
        }];
      case 3:
        _e.sent();
        _e.label = 4;
      case 4:
        _c = _b.next();
        return [3, 2];
      case 5:
        return [3, 8];
      case 6:
        e_2_1 = _e.sent();
        e_2 = { error: e_2_1 };
        return [3, 8];
      case 7:
        try {
          if (_c && !_c.done && (_d = _b.return)) _d.call(_b);
        } finally {
          if (e_2) throw e_2.error;
        }
        return [
          7
          /*endfinally*/
        ];
      case 8:
        return [2, { bounded: false, precedingZeroWidthEntries: [] }];
    }
  });
}
function buildCharacterReaderLevel2(_a) {
  var caseInsensitive = _a.caseInsensitive, dotAll = _a.dotAll, node = _a.node, nodeExtra = _a.nodeExtra;
  var startThread = function(state) {
    var _loop_2, state_1;
    return __generator(this, function(_a2) {
      switch (_a2.label) {
        case 0:
          _loop_2 = function() {
            var precedingZeroWidthEntries, groupsWithInfiniteSize, groupContentsStore, quantifierIterationsAtLastGroup, characterReader, referenceCharacterReaderWithReference, activeCharacterReader, result, value, _b, _groupContentsStore_1, _quantifierIterationsAtLastGroup_1, _groupsWithInfiniteSize_1, _precedingZeroWidthEntries_1, quantifierIterations, lookaheadStack, groups, _c, groupContentsReader, newGroupContentsStore, groupContentsStore_1, groupContentsStore_1_1, _d, index, group, offsets, offsets_1, offsets_1_1, offset, groupInfiniteSize, reversedStack, referenceStackIndex, noneReferenceStackPortion, _loop_3, noneReferenceStackPortion_1, noneReferenceStackPortion_1_1, stackEntry;
            var e_3, _e, e_4, _f, e_5, _g;
            return __generator(this, function(_h) {
              switch (_h.label) {
                case 0:
                  precedingZeroWidthEntries = state.precedingZeroWidthEntries, groupsWithInfiniteSize = state.groupsWithInfiniteSize, groupContentsStore = state.groupContentsStore, quantifierIterationsAtLastGroup = state.quantifierIterationsAtLastGroup;
                  characterReader = state.characterReader, referenceCharacterReaderWithReference = state.referenceCharacterReaderWithReference;
                  activeCharacterReader = (referenceCharacterReaderWithReference === null || referenceCharacterReaderWithReference === void 0 ? void 0 : referenceCharacterReaderWithReference.reader) || characterReader;
                  result = activeCharacterReader.next();
                  if (result.done) {
                    if (referenceCharacterReaderWithReference) {
                      if (result.value.bounded) {
                        throw new Error("Internal error: end of reference reader cannot be bounded");
                      }
                      state = {
                        characterReader,
                        groupContentsStore,
                        groupsWithInfiniteSize,
                        precedingZeroWidthEntries: __spreadArray(__spreadArray([], __read(precedingZeroWidthEntries), false), __read(result.value.precedingZeroWidthEntries), false),
                        quantifierIterationsAtLastGroup,
                        referenceCharacterReaderWithReference: null
                      };
                      return [2, "continue-outer"];
                    }
                    return [2, { value: {
                      bounded: result.value.bounded,
                      precedingZeroWidthEntries: __spreadArray(__spreadArray([], __read(precedingZeroWidthEntries), false), __read(result.value.precedingZeroWidthEntries), false),
                      type: "end"
                    } }];
                  }
                  value = result.value;
                  _b = value.type;
                  switch (_b) {
                    case internalReaderTypeSplit:
                      return [3, 1];
                    case internalReaderTypeEntry:
                      return [3, 3];
                  }
                  return [3, 7];
                case 1:
                  if (referenceCharacterReaderWithReference) {
                    throw new Error("Internal error: should not be seeing a split from a reference reader");
                  }
                  _groupContentsStore_1 = groupContentsStore;
                  _quantifierIterationsAtLastGroup_1 = quantifierIterationsAtLastGroup;
                  _groupsWithInfiniteSize_1 = groupsWithInfiniteSize;
                  _precedingZeroWidthEntries_1 = precedingZeroWidthEntries;
                  return [4, {
                    reader: function() {
                      return startThread({
                        characterReader: buildForkableReader(value.reader()),
                        groupContentsStore: _groupContentsStore_1,
                        groupsWithInfiniteSize: _groupsWithInfiniteSize_1,
                        precedingZeroWidthEntries: _precedingZeroWidthEntries_1,
                        quantifierIterationsAtLastGroup: _quantifierIterationsAtLastGroup_1,
                        referenceCharacterReaderWithReference
                      });
                    },
                    subType: value.subType,
                    type: characterReaderLevel2TypeSplit
                  }];
                case 2:
                  _h.sent();
                  return [3, 7];
                case 3:
                  precedingZeroWidthEntries = __spreadArray(__spreadArray([], __read(precedingZeroWidthEntries), false), __read(value.precedingZeroWidthEntries), false);
                  quantifierIterations = buildQuantifierIterations(value.stack);
                  lookaheadStack = getLookaheadStack(value.stack);
                  groups = getGroups(value.stack);
                  _c = value.subType;
                  switch (_c) {
                    case "reference":
                      return [3, 4];
                    case "groups":
                      return [3, 5];
                  }
                  return [3, 7];
                case 4: {
                  if (referenceCharacterReaderWithReference) {
                    throw new Error("Internal error: should not be seeing a reference from a reference reader");
                  }
                  if (haveHadCompleteIteration(quantifierIterationsAtLastGroup, quantifierIterations)) {
                    return [2, { value: {
                      type: "abort"
                    } }];
                  }
                  groupContentsReader = buildForkableReader(getGroupContentsReader({
                    groupContentsStore,
                    groups,
                    groupsWithInfiniteSize,
                    nodeExtra,
                    value
                  }));
                  state = {
                    characterReader,
                    groupContentsStore,
                    groupsWithInfiniteSize,
                    precedingZeroWidthEntries,
                    quantifierIterationsAtLastGroup,
                    referenceCharacterReaderWithReference: {
                      reader: groupContentsReader,
                      reference: value.node
                    }
                  };
                  return [3, 7];
                }
                case 5:
                  newGroupContentsStore = new Map(groupContentsStore);
                  if (!referenceCharacterReaderWithReference) {
                    try {
                      for (groupContentsStore_1 = (e_3 = void 0, __values(groupContentsStore)), groupContentsStore_1_1 = groupContentsStore_1.next(); !groupContentsStore_1_1.done; groupContentsStore_1_1 = groupContentsStore_1.next()) {
                        _d = __read(groupContentsStore_1_1.value, 2), index = _d[0], group = _d[1].group;
                        offsets = __spreadArray(__spreadArray([], __read(precedingZeroWidthEntries.map(function(_a3) {
                          var offset2 = _a3.offset;
                          return offset2;
                        })), false), [
                          value.node.range[0]
                        ], false);
                        try {
                          for (offsets_1 = (e_4 = void 0, __values(offsets)), offsets_1_1 = offsets_1.next(); !offsets_1_1.done; offsets_1_1 = offsets_1.next()) {
                            offset = offsets_1_1.value;
                            if (group.range[0] >= offset) {
                              newGroupContentsStore.delete(index);
                              break;
                            }
                          }
                        } catch (e_4_1) {
                          e_4 = { error: e_4_1 };
                        } finally {
                          try {
                            if (offsets_1_1 && !offsets_1_1.done && (_f = offsets_1.return)) _f.call(offsets_1);
                          } finally {
                            if (e_4) throw e_4.error;
                          }
                        }
                      }
                    } catch (e_3_1) {
                      e_3 = { error: e_3_1 };
                    } finally {
                      try {
                        if (groupContentsStore_1_1 && !groupContentsStore_1_1.done && (_e = groupContentsStore_1.return)) _e.call(groupContentsStore_1);
                      } finally {
                        if (e_3) throw e_3.error;
                      }
                    }
                  }
                  groupInfiniteSize = false;
                  reversedStack = __spreadArray([], __read(value.stack), false).reverse();
                  referenceStackIndex = reversedStack.findIndex(function(_a3) {
                    var type = _a3.type;
                    return type === "reference";
                  });
                  noneReferenceStackPortion = reversedStack.slice(0, referenceStackIndex >= 0 ? referenceStackIndex : value.stack.length);
                  _loop_3 = function(stackEntry2) {
                    if (stackEntry2.type === "quantifier" && stackEntry2.quantifier.max === void 0) {
                      groupInfiniteSize = true;
                      return "continue";
                    } else if (stackEntry2.type !== "group") {
                      return "continue";
                    }
                    var group2 = stackEntry2.group;
                    if (group2.behavior === "lookbehind" || group2.behavior === "negativeLookbehind" || group2.behavior === "negativeLookahead" || group2.behavior === "lookahead") {
                      groupInfiniteSize = false;
                      return "continue";
                    }
                    if (group2.behavior !== "normal") {
                      return "continue";
                    }
                    var index2 = mustGet(nodeExtra.capturingGroupToIndex, group2);
                    if (groupInfiniteSize) {
                      var newGroupsWithInfiniteSize = new Set(groupsWithInfiniteSize);
                      newGroupsWithInfiniteSize.add(index2);
                      groupsWithInfiniteSize = newGroupsWithInfiniteSize;
                    }
                    var contents = newGroupContentsStore.get(index2) || {
                      contents: [],
                      group: group2
                    };
                    newGroupContentsStore.set(index2, __assign(__assign({}, contents), { contents: __spreadArray(__spreadArray([], __read(contents.contents), false), [
                      __assign(__assign({}, value), { precedingZeroWidthEntries: precedingZeroWidthEntries.filter(function(_a3) {
                        var offset2 = _a3.offset;
                        return offset2 >= group2.range[0] && offset2 <= group2.range[1];
                      }) })
                    ], false) }));
                  };
                  try {
                    for (noneReferenceStackPortion_1 = (e_5 = void 0, __values(noneReferenceStackPortion)), noneReferenceStackPortion_1_1 = noneReferenceStackPortion_1.next(); !noneReferenceStackPortion_1_1.done; noneReferenceStackPortion_1_1 = noneReferenceStackPortion_1.next()) {
                      stackEntry = noneReferenceStackPortion_1_1.value;
                      _loop_3(stackEntry);
                    }
                  } catch (e_5_1) {
                    e_5 = { error: e_5_1 };
                  } finally {
                    try {
                      if (noneReferenceStackPortion_1_1 && !noneReferenceStackPortion_1_1.done && (_g = noneReferenceStackPortion_1.return)) _g.call(noneReferenceStackPortion_1);
                    } finally {
                      if (e_5) throw e_5.error;
                    }
                  }
                  groupContentsStore = newGroupContentsStore;
                  quantifierIterationsAtLastGroup = quantifierIterations;
                  return [4, {
                    characterGroups: value.characterGroups,
                    groups,
                    lookaheadStack,
                    node: value.node,
                    precedingZeroWidthEntries,
                    stack: value.stack,
                    type: characterReaderLevel2TypeEntry
                  }];
                case 6:
                  _h.sent();
                  precedingZeroWidthEntries = [];
                  state = {
                    characterReader,
                    groupContentsStore,
                    groupsWithInfiniteSize,
                    precedingZeroWidthEntries,
                    quantifierIterationsAtLastGroup,
                    referenceCharacterReaderWithReference
                  };
                  return [3, 7];
                case 7:
                  return [
                    2
                    /*return*/
                  ];
              }
            });
          };
          _a2.label = 1;
        case 1:
          return [5, _loop_2()];
        case 2:
          state_1 = _a2.sent();
          if (typeof state_1 === "object")
            return [2, state_1.value];
          switch (state_1) {
            case "continue-outer":
              return [3, 3];
          }
          _a2.label = 3;
        case 3:
          return [3, 1];
        case 4:
          return [
            2
            /*return*/
          ];
      }
    });
  };
  return startThread({
    characterReader: buildForkableReader(characterReaderLevel1ToInternalReader(buildCharacterReaderLevel1({ caseInsensitive, dotAll, node }))),
    groupContentsStore: /* @__PURE__ */ new Map(),
    groupsWithInfiniteSize: /* @__PURE__ */ new Set(),
    precedingZeroWidthEntries: [],
    quantifierIterationsAtLastGroup: /* @__PURE__ */ new Map(),
    referenceCharacterReaderWithReference: null
  });
}
var InfiniteLoopTracker = (
  /** @class */
  (function() {
    function InfiniteLoopTracker2(isEqual, source) {
      this._history = [];
      this._isEqual = isEqual;
      if (source) {
        this._history = __spreadArray([], __read(source.getHistory()), false);
      }
    }
    InfiniteLoopTracker2.prototype._isEntryEqual = function(a4, b4) {
      return this._isEqual(a4.left, b4.left) && this._isEqual(a4.right, b4.right);
    };
    InfiniteLoopTracker2.prototype.append = function(entry) {
      this._history.push(entry);
    };
    InfiniteLoopTracker2.prototype.clone = function() {
      return new InfiniteLoopTracker2(this._isEqual, this);
    };
    InfiniteLoopTracker2.prototype.getHistory = function() {
      return __spreadArray([], __read(this._history), false);
    };
    InfiniteLoopTracker2.prototype.getRepeatingEntries = function() {
      var length = this._history.length;
      outer: for (var candidateSize = 1; candidateSize <= length / 2; candidateSize++) {
        var candidateStart = length - candidateSize * 2;
        for (var i7 = 0; i7 < candidateSize; i7++) {
          if (!this._isEntryEqual(this._history[candidateStart + i7], this._history[candidateStart + candidateSize + i7])) {
            continue outer;
          }
        }
        return this._history.slice(candidateStart, candidateStart + candidateSize);
      }
      return null;
    };
    return InfiniteLoopTracker2;
  })()
);
function once(fn) {
  var done = false;
  var res;
  return function() {
    if (!done) {
      done = true;
      res = fn();
    }
    return res;
  };
}
var isUnboundedReaderTypeStep = /* @__PURE__ */ Symbol("isUnboundedReaderTypeStep");
var notNewLine = {
  ranges: (
    // [\n\r\u2028-\u2029]
    [
      [10, 10],
      [13, 13],
      [8232, 8233]
    ]
  ),
  rangesNegated: true,
  unicodePropertyEscapes: /* @__PURE__ */ new Map()
};
function isUnboundedReader(_a) {
  var reader, stack, _loop_1, state_1;
  var multiLine = _a.multiLine, inputReader = _a.reader;
  return __generator(this, function(_b) {
    switch (_b.label) {
      case 0:
        reader = fork(inputReader);
        stack = [{ get: once(function() {
          return reader.next();
        }), reader }];
        _loop_1 = function() {
          var frame, next, value, splitReader_1, isNewLine;
          return __generator(this, function(_c) {
            switch (_c.label) {
              case 0:
                frame = stack.pop();
                if (!frame)
                  return [2, "break"];
                return [4, {
                  type: isUnboundedReaderTypeStep
                }];
              case 1:
                _c.sent();
                next = frame.get();
                if (next.done) {
                  if (next.value.type === "end" && !next.value.bounded) {
                    return [2, { value: true }];
                  }
                } else {
                  switch (next.value.type) {
                    case characterReaderLevel2TypeSplit: {
                      value = next.value;
                      stack.push({
                        get: once(function() {
                          return frame.reader.next();
                        }),
                        reader: frame.reader
                      });
                      if (value.subType === null) {
                        splitReader_1 = value.reader();
                        stack.push({
                          get: once(function() {
                            return splitReader_1.next();
                          }),
                          reader: splitReader_1
                        });
                      }
                      break;
                    }
                    case characterReaderLevel2TypeEntry: {
                      if (multiLine) {
                        isNewLine = isEmptyCharacterGroups(intersectCharacterGroups(notNewLine, next.value.characterGroups));
                        if (isNewLine) {
                          stack.push({
                            get: once(function() {
                              return frame.reader.next();
                            }),
                            reader: frame.reader
                          });
                        }
                      }
                      break;
                    }
                  }
                }
                return [
                  2
                  /*return*/
                ];
            }
          });
        };
        _b.label = 1;
      case 1:
        return [5, _loop_1()];
      case 2:
        state_1 = _b.sent();
        if (typeof state_1 === "object")
          return [2, state_1.value];
        if (state_1 === "break")
          return [3, 4];
        _b.label = 3;
      case 3:
        return [3, 1];
      case 4:
        return [2, false];
    }
  });
}
function mergeSets(a4, b4) {
  return new Set(__spreadArray(__spreadArray([], __read(a4), false), __read(b4), false));
}
function areSetsEqual(a4, b4) {
  return a4.size === b4.size && mergeSets(a4, b4).size === a4.size;
}
var checkerReaderTypeTrail = /* @__PURE__ */ Symbol("checkerReaderTypeTrail");
function areInfiniteLoopTrackerEntriesEqual(left, right) {
  return left === right;
}
function buildContextTrail(stack, asteriskInfinite) {
  return stack.map(function(entry) {
    switch (entry.type) {
      case "quantifier": {
        return "q:".concat(entry.quantifier.range[0], ":").concat(asteriskInfinite && entry.quantifier.max === void 0 && entry.iteration >= entry.quantifier.min ? "*" : "".concat(entry.iteration));
      }
      case "reference": {
        return "r:".concat(entry.reference.range[0]);
      }
      case "group": {
        return "g:".concat(entry.group.range[0]);
      }
    }
  }).filter(Boolean).join(",");
}
function atomicGroupHashesFromStack(stack, atomicGroupOffsets) {
  var res = /* @__PURE__ */ new Set();
  for (var i7 = 0; i7 < stack.length; i7++) {
    var stackEntry = stack[i7];
    if (stackEntry.type === "group" && atomicGroupOffsets.has(stackEntry.group.range[0])) {
      res.add(buildContextTrail(stack.slice(0, i7 + 1), false));
    }
  }
  return res;
}
function buildZeroWidthHash(zeroWidthEntry) {
  return [
    zeroWidthEntry.type,
    zeroWidthEntry.offset,
    buildContextTrail(zeroWidthEntry.stack, false)
  ].join(":");
}
function getStackSequence(value, hash) {
  return __spreadArray(__spreadArray([], __read(value.precedingZeroWidthEntries.map(function(a4) {
    return {
      hash: ["zw", buildZeroWidthHash(a4)].join(":"),
      stack: a4.stack
    };
  })), false), [
    { hash: ["s", hash].join(":"), stack: value.stack }
  ], false);
}
function areSidesEqual(a4, b4) {
  return a4.hash === b4.hash;
}
function buildCheckerReader(input) {
  var trails, trailEntriesAtStartOfLoop, infiniteLoopTrackerEntryToTrailEntry, stepCount, latestEndTime, timedOut, infinite, initialLeftStreamReader, initialRightStreamReader, stack, _loop_1, state_1, trails_1, trails_1_1, trail, error;
  var e_1, _a;
  return __generator(this, function(_b) {
    switch (_b.label) {
      case 0:
        trails = /* @__PURE__ */ new Set();
        trailEntriesAtStartOfLoop = /* @__PURE__ */ new Set();
        infiniteLoopTrackerEntryToTrailEntry = /* @__PURE__ */ new Map();
        stepCount = 0;
        latestEndTime = Date.now() + input.timeout;
        timedOut = false;
        infinite = false;
        initialLeftStreamReader = buildForkableReader(input.leftStreamReader);
        initialRightStreamReader = buildForkableReader(input.rightStreamReader);
        stack = [
          {
            infiniteLoopTracker: new InfiniteLoopTracker(areInfiniteLoopTrackerEntriesEqual),
            streamReadersWithGetters: [
              {
                get: once(function() {
                  return initialLeftStreamReader.next();
                }),
                reader: initialLeftStreamReader
              },
              {
                get: once(function() {
                  return initialRightStreamReader.next();
                }),
                reader: initialRightStreamReader
              }
            ],
            trail: []
          }
        ];
        _loop_1 = function() {
          var entry, streamReadersWithGetters, infiniteLoopTracker, trail2, nextValues, _loop_2, i7, state_2, _c, leftNextValue, rightNextValue, leftValue, rightValue, leftPassedStartAnchor, rightPassedStartAnchor, somethingPassedStartAnchor, leftLookahead, rightLookahead, intersection, leftContextTrail, rightContextTrail, newEntryLeftHash, newEntryRightHash, lastTrailEntry, atomicGroupsEnteredTogetherLeft, atomicGroupsEnteredTogetherRight, diverged, leftStackSequence, rightStackSequence, i7, leftStack, rightStack, atomicGroupsNow, atomicGroupsNow_1, atomicGroupsNow_1_1, group, atomicGroupsNowLeft, atomicGroupsEnteredTogetherLeft_1, atomicGroupsEnteredTogetherLeft_1_1, group, atomicGroupsNowRight, atomicGroupsEnteredTogetherRight_1, atomicGroupsEnteredTogetherRight_1_1, group, newEntryLeft, newEntryRight, newEntry, leftQuantifiersInInfiniteProportion, leftAndRightIdentical, infiniteLoopTrackerEntry, repeatingEntries, entryAtStartOfLoop, sidesEqual, leftUnboundedReader, leftUnboundedCheckReaderNext, leftUnbounded, rightUnboundedReader, rightUnboundedCheckReaderNext, rightUnbounded, shouldEmitTrail;
          var e_2, _d, e_3, _e, e_4, _f;
          return __generator(this, function(_g) {
            switch (_g.label) {
              case 0:
                timedOut = Date.now() > latestEndTime;
                if (timedOut || stepCount > input.maxSteps) {
                  return [2, "break"];
                }
                entry = stack.pop();
                if (!entry)
                  return [2, "break"];
                streamReadersWithGetters = entry.streamReadersWithGetters;
                infiniteLoopTracker = entry.infiniteLoopTracker;
                trail2 = entry.trail;
                nextValues = [];
                _loop_2 = function(i8) {
                  stepCount += 0.5;
                  var result = streamReadersWithGetters[i8].get();
                  if (!result.done && result.value.type === characterReaderLevel2TypeSplit) {
                    var value_1 = result.value;
                    stack.push({
                      infiniteLoopTracker,
                      streamReadersWithGetters: streamReadersWithGetters.map(function(_a2, j3) {
                        var reader = _a2.reader, get = _a2.get;
                        return {
                          get: j3 === i8 ? once(function() {
                            return reader.next();
                          }) : get,
                          reader
                        };
                      }),
                      trail: trail2
                    });
                    var newStreamReadersWithGetters = streamReadersWithGetters.map(function(_a2, j3) {
                      var reader = _a2.reader, get = _a2.get;
                      var newReader = j3 === i8 ? buildForkableReader(value_1.reader()) : fork(reader);
                      return {
                        get: j3 < i8 ? get : once(function() {
                          return newReader.next();
                        }),
                        reader: newReader
                      };
                    });
                    stack.push({
                      infiniteLoopTracker: infiniteLoopTracker.clone(),
                      streamReadersWithGetters: newStreamReadersWithGetters,
                      trail: trail2
                    });
                    return "continue-outer";
                  } else {
                    nextValues.push(result);
                  }
                };
                for (i7 = 0; i7 < streamReadersWithGetters.length; i7++) {
                  state_2 = _loop_2(i7);
                  switch (state_2) {
                    case "continue-outer":
                      return [2, state_2];
                  }
                }
                _c = __read(nextValues, 2), leftNextValue = _c[0], rightNextValue = _c[1];
                if (stepCount > input.maxSteps || leftNextValue.done || rightNextValue.done) {
                  return [2, "continue"];
                }
                if (leftNextValue.value.type !== characterReaderLevel2TypeEntry || rightNextValue.value.type !== characterReaderLevel2TypeEntry) {
                  throw new Error("Internal error: impossible leftValue/rightValue type");
                }
                leftValue = leftNextValue.value;
                rightValue = rightNextValue.value;
                leftPassedStartAnchor = leftValue.precedingZeroWidthEntries.some(function(_a2) {
                  var type = _a2.type;
                  return type === "start";
                });
                rightPassedStartAnchor = rightValue.precedingZeroWidthEntries.some(function(_a2) {
                  var type = _a2.type;
                  return type === "start";
                });
                somethingPassedStartAnchor = leftPassedStartAnchor || rightPassedStartAnchor;
                if (trail2.length > 0 && somethingPassedStartAnchor) {
                  return [2, "continue"];
                }
                leftLookahead = last(leftValue.lookaheadStack);
                rightLookahead = last(rightValue.lookaheadStack);
                if (leftLookahead !== rightLookahead) {
                  return [2, "continue"];
                }
                intersection = intersectCharacterGroups(leftValue.characterGroups, rightValue.characterGroups);
                if (isEmptyCharacterGroups(intersection)) {
                  return [2, "continue"];
                }
                leftContextTrail = buildContextTrail(leftValue.stack, false);
                rightContextTrail = buildContextTrail(rightValue.stack, false);
                newEntryLeftHash = [leftValue.node.range[0], leftContextTrail].join(":");
                newEntryRightHash = [
                  rightValue.node.range[0],
                  rightContextTrail
                ].join(":");
                lastTrailEntry = last(trail2);
                atomicGroupsEnteredTogetherLeft = new Set(lastTrailEntry === null || lastTrailEntry === void 0 ? void 0 : lastTrailEntry.left.atomicGroupsEnteredTogether);
                atomicGroupsEnteredTogetherRight = new Set(lastTrailEntry === null || lastTrailEntry === void 0 ? void 0 : lastTrailEntry.right.atomicGroupsEnteredTogether);
                diverged = !!(lastTrailEntry === null || lastTrailEntry === void 0 ? void 0 : lastTrailEntry.diverged);
                if (!diverged) {
                  leftStackSequence = getStackSequence(leftValue, newEntryLeftHash);
                  rightStackSequence = getStackSequence(rightValue, newEntryRightHash);
                  for (
                    i7 = 0;
                    // if they are different lengths the last hashes should never match because will be comparing a zero width with normal entry
                    i7 < Math.min(leftStackSequence.length, rightStackSequence.length);
                    i7++
                  ) {
                    leftStack = leftStackSequence[i7];
                    rightStack = rightStackSequence[i7];
                    if (leftStack.hash !== rightStack.hash) {
                      diverged = true;
                      break;
                    }
                    atomicGroupsNow = atomicGroupHashesFromStack(leftStack.stack, input.atomicGroupOffsets);
                    try {
                      for (atomicGroupsNow_1 = (e_2 = void 0, __values(atomicGroupsNow)), atomicGroupsNow_1_1 = atomicGroupsNow_1.next(); !atomicGroupsNow_1_1.done; atomicGroupsNow_1_1 = atomicGroupsNow_1.next()) {
                        group = atomicGroupsNow_1_1.value;
                        atomicGroupsEnteredTogetherLeft.add(group);
                        atomicGroupsEnteredTogetherRight.add(group);
                      }
                    } catch (e_2_1) {
                      e_2 = { error: e_2_1 };
                    } finally {
                      try {
                        if (atomicGroupsNow_1_1 && !atomicGroupsNow_1_1.done && (_d = atomicGroupsNow_1.return)) _d.call(atomicGroupsNow_1);
                      } finally {
                        if (e_2) throw e_2.error;
                      }
                    }
                  }
                }
                atomicGroupsNowLeft = atomicGroupHashesFromStack(leftValue.stack, input.atomicGroupOffsets);
                try {
                  for (atomicGroupsEnteredTogetherLeft_1 = (e_3 = void 0, __values(atomicGroupsEnteredTogetherLeft)), atomicGroupsEnteredTogetherLeft_1_1 = atomicGroupsEnteredTogetherLeft_1.next(); !atomicGroupsEnteredTogetherLeft_1_1.done; atomicGroupsEnteredTogetherLeft_1_1 = atomicGroupsEnteredTogetherLeft_1.next()) {
                    group = atomicGroupsEnteredTogetherLeft_1_1.value;
                    if (!atomicGroupsNowLeft.has(group)) {
                      atomicGroupsEnteredTogetherLeft.delete(group);
                    }
                  }
                } catch (e_3_1) {
                  e_3 = { error: e_3_1 };
                } finally {
                  try {
                    if (atomicGroupsEnteredTogetherLeft_1_1 && !atomicGroupsEnteredTogetherLeft_1_1.done && (_e = atomicGroupsEnteredTogetherLeft_1.return)) _e.call(atomicGroupsEnteredTogetherLeft_1);
                  } finally {
                    if (e_3) throw e_3.error;
                  }
                }
                atomicGroupsNowRight = atomicGroupHashesFromStack(rightValue.stack, input.atomicGroupOffsets);
                try {
                  for (atomicGroupsEnteredTogetherRight_1 = (e_4 = void 0, __values(atomicGroupsEnteredTogetherRight)), atomicGroupsEnteredTogetherRight_1_1 = atomicGroupsEnteredTogetherRight_1.next(); !atomicGroupsEnteredTogetherRight_1_1.done; atomicGroupsEnteredTogetherRight_1_1 = atomicGroupsEnteredTogetherRight_1.next()) {
                    group = atomicGroupsEnteredTogetherRight_1_1.value;
                    if (!atomicGroupsNowRight.has(group)) {
                      atomicGroupsEnteredTogetherRight.delete(group);
                    }
                  }
                } catch (e_4_1) {
                  e_4 = { error: e_4_1 };
                } finally {
                  try {
                    if (atomicGroupsEnteredTogetherRight_1_1 && !atomicGroupsEnteredTogetherRight_1_1.done && (_f = atomicGroupsEnteredTogetherRight_1.return)) _f.call(atomicGroupsEnteredTogetherRight_1);
                  } finally {
                    if (e_4) throw e_4.error;
                  }
                }
                newEntryLeft = {
                  atomicGroupsEnteredTogether: atomicGroupsEnteredTogetherLeft,
                  characterGroups: leftValue.characterGroups,
                  contextTrail: leftContextTrail,
                  hash: newEntryLeftHash,
                  node: leftValue.node,
                  stack: leftValue.stack
                };
                newEntryRight = {
                  atomicGroupsEnteredTogether: atomicGroupsEnteredTogetherRight,
                  characterGroups: rightValue.characterGroups,
                  contextTrail: rightContextTrail,
                  hash: newEntryRightHash,
                  node: rightValue.node,
                  stack: rightValue.stack
                };
                newEntry = {
                  diverged,
                  intersection,
                  left: newEntryLeft,
                  right: newEntryRight
                };
                trail2 = __spreadArray(__spreadArray([], __read(trail2), false), [newEntry], false);
                leftQuantifiersInInfiniteProportion = buildQuantifiersInInfinitePortion(leftValue.stack, input.nodeExtra);
                if (leftQuantifiersInInfiniteProportion.size > 0) {
                  leftAndRightIdentical = trail2.every(function(_a2) {
                    var left = _a2.left, right = _a2.right;
                    return areSidesEqual(left, right);
                  });
                  if (leftAndRightIdentical) {
                    return [2, "continue"];
                  }
                }
                infiniteLoopTrackerEntry = {
                  left: [
                    leftValue.node.range[0],
                    buildContextTrail(leftValue.stack, true)
                  ].join(":"),
                  right: [
                    rightValue.node.range[0],
                    buildContextTrail(leftValue.stack, true)
                  ].join(":")
                };
                infiniteLoopTrackerEntryToTrailEntry.set(infiniteLoopTrackerEntry, newEntry);
                infiniteLoopTracker.append(infiniteLoopTrackerEntry);
                repeatingEntries = infiniteLoopTracker.getRepeatingEntries();
                if (repeatingEntries) {
                  entryAtStartOfLoop = infiniteLoopTrackerEntryToTrailEntry.get(repeatingEntries[0]);
                  if (!entryAtStartOfLoop) {
                    throw new Error("Internal error: missing entry at start of loop");
                  }
                  trailEntriesAtStartOfLoop.add(entryAtStartOfLoop);
                  return [2, "continue"];
                }
                if (!areSetsEqual(newEntry.left.atomicGroupsEnteredTogether, newEntry.right.atomicGroupsEnteredTogether)) {
                  return [2, "continue-outer"];
                }
                sidesEqual = areSidesEqual(newEntry.left, newEntry.right);
                if (!!sidesEqual) return [3, 2];
                {
                  leftUnboundedReader = isUnboundedReader({
                    multiLine: input.multiLine,
                    reader: fork(streamReadersWithGetters[0].reader)
                  });
                  leftUnboundedCheckReaderNext = void 0;
                  while (!(leftUnboundedCheckReaderNext = leftUnboundedReader.next()).done) {
                    switch (leftUnboundedCheckReaderNext.value.type) {
                      case isUnboundedReaderTypeStep: {
                        stepCount += 0.5;
                        if (stepCount > input.maxSteps) {
                          return [2, "break-outer"];
                        }
                        break;
                      }
                    }
                  }
                  leftUnbounded = leftUnboundedCheckReaderNext.value;
                  if (leftUnbounded)
                    return [2, "continue"];
                }
                {
                  rightUnboundedReader = isUnboundedReader({
                    multiLine: input.multiLine,
                    reader: fork(streamReadersWithGetters[1].reader)
                  });
                  rightUnboundedCheckReaderNext = void 0;
                  while (!(rightUnboundedCheckReaderNext = rightUnboundedReader.next()).done) {
                    switch (rightUnboundedCheckReaderNext.value.type) {
                      case isUnboundedReaderTypeStep: {
                        stepCount += 0.5;
                        if (stepCount > input.maxSteps) {
                          return [2, "break-outer"];
                        }
                        break;
                      }
                    }
                  }
                  rightUnbounded = rightUnboundedCheckReaderNext.value;
                  if (rightUnbounded)
                    return [2, "continue"];
                }
                shouldEmitTrail = function() {
                  var alreadyExists = __spreadArray([], __read(trails), false).some(function(existingTrail) {
                    if (existingTrail.length !== trail2.length)
                      return false;
                    return existingTrail.every(function(existingEntry, i8) {
                      return areSidesEqual(existingEntry.left, trail2[i8].left) && areSidesEqual(existingEntry.right, trail2[i8].right);
                    }) || existingTrail.every(function(existingEntry, i8) {
                      return areSidesEqual(existingEntry.left, trail2[i8].right) && areSidesEqual(existingEntry.right, trail2[i8].left);
                    });
                  });
                  return !alreadyExists;
                };
                if (!shouldEmitTrail()) return [3, 2];
                trails.add(trail2);
                return [4, { trail: trail2, type: checkerReaderTypeTrail }];
              case 1:
                _g.sent();
                _g.label = 2;
              case 2:
                stack.push({
                  infiniteLoopTracker,
                  streamReadersWithGetters: streamReadersWithGetters.map(function(_a2) {
                    var reader = _a2.reader;
                    return {
                      get: once(function() {
                        return reader.next();
                      }),
                      reader
                    };
                  }),
                  trail: trail2
                });
                return [
                  2
                  /*return*/
                ];
            }
          });
        };
        _b.label = 1;
      case 1:
        return [5, _loop_1()];
      case 2:
        state_1 = _b.sent();
        if (state_1 === "break")
          return [3, 4];
        switch (state_1) {
          case "break-outer":
            return [3, 4];
          case "continue-outer":
            return [3, 3];
        }
        _b.label = 3;
      case 3:
        return [3, 1];
      case 4:
        if (trailEntriesAtStartOfLoop.size > 0) {
          try {
            for (trails_1 = __values(trails), trails_1_1 = trails_1.next(); !trails_1_1.done; trails_1_1 = trails_1.next()) {
              trail = trails_1_1.value;
              if (trail.some(function(entry) {
                return trailEntriesAtStartOfLoop.has(entry);
              })) {
                infinite = true;
                break;
              }
            }
          } catch (e_1_1) {
            e_1 = { error: e_1_1 };
          } finally {
            try {
              if (trails_1_1 && !trails_1_1.done && (_a = trails_1.return)) _a.call(trails_1);
            } finally {
              if (e_1) throw e_1.error;
            }
          }
        }
        error = stepCount > input.maxSteps ? "hitMaxSteps" : timedOut ? "timedOut" : null;
        return [2, error ? { error } : { error: null, infinite }];
    }
  });
}
var lookaheadBehaviours = [
  "lookahead",
  "lookbehind",
  "negativeLookahead",
  "negativeLookbehind"
];
function buildNodeExtra(regexp) {
  var capturingGroupToIndex = /* @__PURE__ */ new Map();
  var indexToCapturingGroup = /* @__PURE__ */ new Map();
  var nodeToLookaheadStack = /* @__PURE__ */ new Map();
  var reachableReferences = [];
  var visit = function(node, lookaheadStack, reachable) {
    switch (node.type) {
      case "anchor":
      case "characterClass":
      case "characterClassEscape":
      case "characterClassRange":
      case "unicodePropertyEscape":
      case "value":
      case "dot":
        return;
      case "reference": {
        nodeToLookaheadStack.set(node, lookaheadStack);
        if (reachable)
          reachableReferences.push(node);
        break;
      }
      case "alternative":
      case "disjunction": {
        node.body.forEach(function(expression) {
          return visit(expression, lookaheadStack, reachable);
        });
        return;
      }
      case "group": {
        if (node.behavior === "normal") {
          var index = capturingGroupToIndex.size + 1;
          capturingGroupToIndex.set(node, index);
          indexToCapturingGroup.set(index, node);
          nodeToLookaheadStack.set(node, lookaheadStack);
        }
        var newLookaheadStack_1 = __spreadArray([], __read(lookaheadStack), false);
        if (node.behavior !== "normal" && lookaheadBehaviours.indexOf(node.behavior) >= 0) {
          newLookaheadStack_1.push(node);
        }
        node.body.forEach(function(expression) {
          return visit(expression, newLookaheadStack_1, reachable);
        });
        return;
      }
      case "quantifier": {
        node.body.forEach(function(expression) {
          return visit(expression, lookaheadStack, reachable && node.max !== 0);
        });
        return;
      }
    }
  };
  visit(regexp, [], true);
  return {
    capturingGroupToIndex,
    indexToCapturingGroup,
    nodeToLookaheadStack,
    reachableReferences
  };
}
var ResultCache = (
  /** @class */
  (function() {
    function ResultCache2() {
      this._cache = /* @__PURE__ */ new Map();
    }
    ResultCache2.prototype.addResult = function(a4, b4, result) {
      var mapA = this._cache.get(a4) || /* @__PURE__ */ new Map();
      mapA.set(b4, result);
      this._cache.set(a4, mapA);
      var mapB = this._cache.get(b4) || /* @__PURE__ */ new Map();
      mapB.set(a4, result);
      this._cache.set(b4, mapB);
    };
    ResultCache2.prototype.getResult = function(a4, b4) {
      var _a;
      return (_a = this._cache.get(a4)) === null || _a === void 0 ? void 0 : _a.get(b4);
    };
    return ResultCache2;
  })()
);
var Tree = (
  /** @class */
  (function() {
    function Tree2(decode) {
      this.decode = decode;
      this.root = /* @__PURE__ */ new Map();
      this.results = /* @__PURE__ */ new Map();
      this._items = function() {
        return [];
      };
    }
    Tree2.prototype.add = function(input) {
      var values = this.decode(input);
      if (values.length === 0)
        return;
      var results = this.results;
      var current = {
        children: this.root
      };
      var numValues = values.length;
      for (var i7 = 0; i7 < numValues; i7++) {
        var value = values[i7];
        var last2 = i7 === numValues - 1;
        var existing = current.children.get(value);
        if (existing) {
          current = existing;
          if (!last2) {
            results.delete(existing);
          }
        } else {
          var newChild = { children: /* @__PURE__ */ new Map(), value };
          current.children.set(value, newChild);
          current = newChild;
          if (last2) {
            results.set(newChild, input);
          }
        }
      }
      this._items = once(function() {
        return Array.from(results.values());
      });
    };
    Object.defineProperty(Tree2.prototype, "items", {
      get: function() {
        return this._items();
      },
      enumerable: false,
      configurable: true
    });
    return Tree2;
  })()
);
var workLimit = 25e3;
var EnhancedTrail = (
  /** @class */
  (function() {
    function EnhancedTrail2(trail, isEmptyCache) {
      this.trail = trail;
      this.isEmptyCache = isEmptyCache;
      this.tree = new Tree(function(trail2) {
        return trail2.map(function(_a) {
          var hash = _a.hash;
          return hash;
        });
      });
      this.inputStringSchema = trail.map(function(_a) {
        var intersection = _a.intersection;
        return intersection;
      });
      this.length = trail.length;
      this.onNewTrail(this);
    }
    Object.defineProperty(EnhancedTrail2.prototype, "matches", {
      get: function() {
        return this.tree.items;
      },
      enumerable: false,
      configurable: true
    });
    EnhancedTrail2.prototype.getLongestMatch = function(inputStringSchema, trailSides) {
      var _this = this;
      if (trailSides.length > inputStringSchema.length) {
        throw new Error("Internal error: trail should be <= than input string schema");
      }
      var noMatchOffset = trailSides.findIndex(function(side, i7) {
        var res = _this.isEmptyCache.getResult(side.characterGroups, inputStringSchema[i7]);
        if (res !== void 0)
          return res;
        var isEmpty = isEmptyCharacterGroups(intersectCharacterGroups(side.characterGroups, inputStringSchema[i7]));
        _this.isEmptyCache.addResult(side.characterGroups, inputStringSchema[i7], isEmpty);
        return isEmpty;
      });
      return noMatchOffset === -1 ? trailSides : trailSides.slice(0, noMatchOffset);
    };
    EnhancedTrail2.prototype.onNewTrail = function(otherTrail) {
      var e_1, _a;
      var leftSide = [];
      var rightSide = [];
      try {
        for (var _b = __values(otherTrail.trail), _c = _b.next(); !_c.done; _c = _b.next()) {
          var entry = _c.value;
          leftSide.push(entry.left);
          rightSide.push(entry.right);
        }
      } catch (e_1_1) {
        e_1 = { error: e_1_1 };
      } finally {
        try {
          if (_c && !_c.done && (_a = _b.return)) _a.call(_b);
        } finally {
          if (e_1) throw e_1.error;
        }
      }
      var leftMatch = this.getLongestMatch(this.inputStringSchema, leftSide);
      if (leftMatch.length === this.length)
        this.tree.add(leftMatch);
      var rightMatch = this.getLongestMatch(this.inputStringSchema, rightSide);
      if (rightMatch.length === this.length)
        this.tree.add(rightMatch);
    };
    return EnhancedTrail2;
  })()
);
function collectResults(_a) {
  var e_2, _b;
  var atomicGroupOffsets = _a.atomicGroupOffsets, node = _a.node, maxScore = _a.maxScore, maxSteps = _a.maxSteps, multiLine = _a.multiLine, timeout = _a.timeout, caseInsensitive = _a.caseInsensitive, dotAll = _a.dotAll;
  var isEmptyCache = new ResultCache();
  var nodeExtra = buildNodeExtra(node);
  var input = {
    caseInsensitive,
    dotAll,
    node,
    nodeExtra
  };
  var leftStreamReader = buildCharacterReaderLevel2(input);
  var rightStreamReader = buildCharacterReaderLevel2(input);
  var reader = buildCheckerReader({
    atomicGroupOffsets,
    leftStreamReader,
    maxSteps,
    multiLine,
    nodeExtra,
    rightStreamReader,
    timeout
  });
  var trailsTree = new Tree(function(_a2) {
    var trail2 = _a2.trail;
    return trail2;
  });
  var score = 1;
  var work = 0;
  var next;
  outer: while (!(next = reader.next()).done) {
    switch (next.value.type) {
      case checkerReaderTypeTrail: {
        var trail = new EnhancedTrail(next.value.trail, isEmptyCache);
        var updateScore = function(_a2) {
          var matches = _a2.matches;
          var length = matches.length;
          if (length > score) {
            score = length;
          }
        };
        if (work < workLimit) {
          try {
            for (var _c = (e_2 = void 0, __values(trailsTree.items)), _d = _c.next(); !_d.done; _d = _c.next()) {
              var existingTrail = _d.value;
              if (existingTrail.length === trail.length) {
                work += trail.length;
                if (work >= workLimit)
                  break;
                trail.onNewTrail(existingTrail);
                existingTrail.onNewTrail(trail);
                updateScore(existingTrail);
              }
            }
          } catch (e_2_1) {
            e_2 = { error: e_2_1 };
          } finally {
            try {
              if (_d && !_d.done && (_b = _c.return)) _b.call(_c);
            } finally {
              if (e_2) throw e_2.error;
            }
          }
          updateScore(trail);
        }
        if (work >= workLimit) {
          score += 1;
        }
        trailsTree.add(trail);
        if (score > maxScore) {
          break outer;
        }
        break;
      }
    }
  }
  var error = null;
  if (next.done) {
    if (next.value.error) {
      score = Infinity;
      error = next.value.error;
    } else if (next.value.infinite) {
      score = Infinity;
      error = "hitMaxScore";
    }
  } else {
    score = Infinity;
    error = "hitMaxScore";
  }
  return {
    error,
    score,
    trails: trailsTree.items.map(function(_a2) {
      var trail2 = _a2.trail;
      return trail2;
    })
  };
}
var defaultTimeout = Infinity;
var defaultMaxScore = 200;
var defaultMaxSteps = 2e4;
var defaultMultiLine = false;
var defaultUnicode = false;
var defaultCaseInsensitive = false;
var defaultDotAll = false;
function toRedosDetectorNode(node) {
  return {
    end: { offset: node.range[1] },
    source: node.raw,
    start: { offset: node.range[0] }
  };
}
function toRedosDetectorBackReferenceStack(stack) {
  return stack.flatMap(function(stackEntry) {
    return stackEntry.type === "reference" ? [stackEntry.reference] : [];
  }).reverse().map(function(reference) {
    return {
      index: reference.matchIndex,
      node: toRedosDetectorNode(reference)
    };
  });
}
function toRedosDetectorQuantifierIterations(stack) {
  var reversedStack = __spreadArray([], __read(stack), false).reverse();
  var referenceStackIndex = reversedStack.findIndex(function(_a) {
    var type = _a.type;
    return type === "reference";
  });
  var noneReferenceStackPortion = reversedStack.slice(0, referenceStackIndex >= 0 ? referenceStackIndex : stack.length).reverse();
  return noneReferenceStackPortion.flatMap(function(entry) {
    return entry.type === "quantifier" ? [entry] : [];
  }).map(function(_a) {
    var quantifier = _a.quantifier, iteration = _a.iteration;
    return {
      iteration,
      node: toRedosDetectorNode(quantifier)
    };
  });
}
function isSafePattern(inputPattern, _a) {
  var _b = _a === void 0 ? {} : _a, atomicGroupOffsetsInput = _b.atomicGroupOffsets, _c = _b.maxScore, maxScore = _c === void 0 ? defaultMaxScore : _c, _d = _b.maxSteps, maxSteps = _d === void 0 ? defaultMaxSteps : _d, _e = _b.multiLine, multiLine = _e === void 0 ? defaultMultiLine : _e, _f = _b.timeout, timeout = _f === void 0 ? defaultTimeout : _f, _g = _b.caseInsensitive, caseInsensitive = _g === void 0 ? defaultCaseInsensitive : _g, _h = _b.dotAll, dotAll = _h === void 0 ? defaultDotAll : _h, _j = _b.unicode, unicode = _j === void 0 ? defaultUnicode : _j, _k = _b.downgradePattern, downgradePattern$1 = _k === void 0 ? true : _k;
  if (caseInsensitive && unicode) {
    throw new Error("`caseInsensitive` cannot be used with `unicode`.");
  }
  if (downgradePattern$1 && atomicGroupOffsetsInput) {
    throw new Error("`atomicGroupOffsets` cannot be used with `downgradePattern: true`.");
  }
  if (timeout <= 0) {
    throw new Error("`timeout` must be a positive number.");
  }
  if (maxScore < 0) {
    throw new Error("`maxScore` must be a positive number or 0.");
  }
  if (maxSteps <= 0) {
    throw new Error("`maxSteps` must be a positive number.");
  }
  var _l = downgradePattern$1 ? downgradePattern({ pattern: inputPattern, unicode }) : {
    atomicGroupOffsets: new Set(atomicGroupOffsetsInput || []),
    pattern: inputPattern
  }, pattern = _l.pattern, atomicGroupOffsets = _l.atomicGroupOffsets;
  var patternDowngraded = downgradePattern$1 && inputPattern !== pattern;
  var ast = parse(pattern, unicode);
  if (!downgradePattern$1 && isMissingStartAnchor(ast)) {
    throw new Error("Pattern is not bounded at the start and needs downgrading. See the `downgradePattern` option.");
  }
  var result = collectResults({
    atomicGroupOffsets,
    caseInsensitive,
    dotAll,
    maxScore,
    maxSteps,
    multiLine,
    node: ast,
    timeout
  });
  return __assign(__assign({}, result.error ? {
    error: result.error,
    safe: false
  } : {
    error: null,
    safe: true
  }), { pattern, patternDowngraded, score: result.score === Infinity ? { infinite: true } : { infinite: false, value: result.score }, trails: result.trails.map(function(trail) {
    var safeRegexTrail = {
      trail: trail.map(function(_a2) {
        var left = _a2.left, right = _a2.right;
        var entry = {
          a: {
            backreferenceStack: toRedosDetectorBackReferenceStack(right.stack),
            node: toRedosDetectorNode(right.node),
            quantifierIterations: toRedosDetectorQuantifierIterations(right.stack)
          },
          b: {
            backreferenceStack: toRedosDetectorBackReferenceStack(left.stack),
            node: toRedosDetectorNode(left.node),
            quantifierIterations: toRedosDetectorQuantifierIterations(left.stack)
          }
        };
        return entry;
      })
    };
    return safeRegexTrail;
  }) });
}

// node_modules/@a2ui/web_core/src/v0_9/basic_catalog/functions/safe_regex.js
function isSafeRegex(pattern, options = {}) {
  if (pattern === null || pattern === void 0 || typeof pattern !== "string") {
    return true;
  }
  if (pattern.length === 0) {
    return true;
  }
  const maxPatternLength = options.maxPatternLength ?? 256;
  if (pattern.length > maxPatternLength) {
    return false;
  }
  try {
    new RegExp(pattern);
    return isSafePattern(pattern).safe && isSafePattern(`^(?:${pattern})$`).safe;
  } catch {
    return false;
  }
}

// node_modules/@a2ui/web_core/src/common/basic_functions.js
var DEFAULT_LOCALE = "en-US";
function resolveLocale(locale) {
  if (!locale)
    return DEFAULT_LOCALE;
  try {
    return Intl.NumberFormat.supportedLocalesOf(locale).length > 0 ? locale : DEFAULT_LOCALE;
  } catch {
    return DEFAULT_LOCALE;
  }
}
function coerceToString(value) {
  if (value === null || value === void 0)
    return "";
  if (typeof value === "object") {
    try {
      return JSON.stringify(value) ?? String(value);
    } catch {
      return String(value);
    }
  }
  return String(value);
}
function validateRequired(val) {
  let isValid2 = true;
  if (val === null || val === void 0)
    isValid2 = false;
  else if (typeof val === "string" && val === "")
    isValid2 = false;
  else if (Array.isArray(val) && val.length === 0)
    isValid2 = false;
  return {
    valid: isValid2,
    ...isValid2 ? {} : { message: "This field is required." }
  };
}
function validateRegex(val, pattern) {
  if (!isSafeRegex(pattern)) {
    throw new A2uiExpressionError(`Unsafe regex pattern (catastrophic backtracking risk): ${pattern}`, "regex");
  }
  try {
    const isValid2 = new RegExp(pattern).test(String(val ?? ""));
    return {
      valid: isValid2,
      ...isValid2 ? {} : { message: "Value does not match required pattern." }
    };
  } catch (e7) {
    throw new A2uiExpressionError(`Invalid regex pattern: ${pattern}`, "regex", e7);
  }
}
function validateLength(val, min, max) {
  let len = 0;
  if (typeof val === "string" || Array.isArray(val)) {
    len = val.length;
  }
  let isValid2 = true;
  let message;
  if (min !== void 0 && !isNaN(min) && len < min) {
    isValid2 = false;
    message = `Minimum length is ${min}.`;
  } else if (max !== void 0 && !isNaN(max) && len > max) {
    isValid2 = false;
    message = `Maximum length is ${max}.`;
  }
  return {
    valid: isValid2,
    ...message ? { message } : {}
  };
}
function validateNumeric(val, min, max) {
  const num = Number(val);
  if (isNaN(num)) {
    return { valid: false, message: "Value must be a valid number." };
  }
  let isValid2 = true;
  let message;
  if (min !== void 0 && !isNaN(min) && num < min) {
    isValid2 = false;
    message = `Minimum value is ${min}.`;
  } else if (max !== void 0 && !isNaN(max) && num > max) {
    isValid2 = false;
    message = `Maximum value is ${max}.`;
  }
  return {
    valid: isValid2,
    ...message ? { message } : {}
  };
}
var EMAIL_REGEX = /^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$/;
function validateEmail(val) {
  const isValid2 = typeof val === "string" && EMAIL_REGEX.test(val);
  return {
    valid: isValid2,
    ...isValid2 ? {} : {
      code: "INVALID_EMAIL",
      severity: "error",
      message: "Please enter a valid email address (e.g. user@example.com)."
    }
  };
}
var numberFormatCache = /* @__PURE__ */ new Map();
function getNumberFormat(locale, decimals, grouping) {
  const key = `${locale}:${decimals ?? "undef"}:${grouping ?? "true"}`;
  let formatter = numberFormatCache.get(key);
  if (!formatter) {
    formatter = new Intl.NumberFormat(locale, {
      minimumFractionDigits: decimals,
      maximumFractionDigits: decimals,
      useGrouping: grouping
    });
    numberFormatCache.set(key, formatter);
  }
  return formatter;
}
var currencyFormatCache = /* @__PURE__ */ new Map();
function getCurrencyFormat(locale, currency, decimals, grouping) {
  const key = `${locale}:${currency}:${decimals ?? "undef"}:${grouping ?? "true"}`;
  let formatter = currencyFormatCache.get(key);
  if (!formatter) {
    formatter = new Intl.NumberFormat(locale, {
      style: "currency",
      currency,
      minimumFractionDigits: decimals,
      maximumFractionDigits: decimals,
      useGrouping: grouping
    });
    currencyFormatCache.set(key, formatter);
  }
  return formatter;
}
var PLACEHOLDER_CURRENCY = "USD";
var currencyCodeFormatCache = /* @__PURE__ */ new Map();
function getCurrencyCodeFormat(locale, decimals, grouping) {
  const key = `${locale}:${decimals ?? "undef"}:${grouping ?? "true"}`;
  let formatter = currencyCodeFormatCache.get(key);
  if (!formatter) {
    formatter = new Intl.NumberFormat(locale, {
      style: "currency",
      currency: PLACEHOLDER_CURRENCY,
      currencyDisplay: "code",
      minimumFractionDigits: decimals,
      maximumFractionDigits: decimals,
      useGrouping: grouping
    });
    currencyCodeFormatCache.set(key, formatter);
  }
  return formatter;
}
var dateNamesCache = /* @__PURE__ */ new Map();
function getDateNames(locale) {
  let names = dateNamesCache.get(locale);
  if (!names) {
    const render = (options, dates) => {
      const formatter = new Intl.DateTimeFormat(locale, { ...options, timeZone: "UTC" });
      return dates.map((d4) => formatter.format(d4));
    };
    const months = Array.from({ length: 12 }, (_3, i7) => new Date(Date.UTC(2021, i7, 15)));
    const weekdays = Array.from({ length: 7 }, (_3, i7) => new Date(Date.UTC(2021, 2, 1 + i7)));
    names = {
      monthsLong: render({ month: "long" }, months),
      monthsShort: render({ month: "short" }, months),
      weekdaysLong: render({ weekday: "long" }, weekdays),
      weekdaysShort: render({ weekday: "short" }, weekdays)
    };
    dateNamesCache.set(locale, names);
  }
  return names;
}
var DATE_TOKENS = /yyyy|yy|MMMM|MMM|MM|M|EEEE|E|dd|d|HH|H|hh|h|mm|ss|a/g;
var ISO_OFFSET = /(?:Z|[+-]\d{2}:?\d{2})$/;
var WRITTEN_FIELDS = /^([+-]?\d{4,6})-(\d{2})-(\d{2})(?:[T ](\d{2})(?::(\d{2})(?::(\d{2}))?)?)?/;
function parseTimestamp(value) {
  const hasOffset = ISO_OFFSET.test(value);
  const instant = new Date(hasOffset ? value : `${value}Z`);
  if (isNaN(instant.getTime()))
    return null;
  let offsetMinutes = 0;
  const match = /([+-])(\d{2}):?(\d{2})$/.exec(value);
  if (match) {
    const sign = match[1] === "-" ? -1 : 1;
    offsetMinutes = sign * (Number(match[2]) * 60 + Number(match[3]));
  }
  const shifted = new Date(instant.getTime() + offsetMinutes * 6e4);
  if (!fieldsRoundTrip(value, shifted))
    return null;
  return { shifted, instant };
}
function fieldsRoundTrip(value, shifted) {
  const written = WRITTEN_FIELDS.exec(value);
  if (!written)
    return false;
  const field = (group) => written[group] === void 0 ? void 0 : Number(written[group]);
  return field(1) === shifted.getUTCFullYear() && field(2) === shifted.getUTCMonth() + 1 && field(3) === shifted.getUTCDate() && (field(4) ?? shifted.getUTCHours()) === shifted.getUTCHours() && (field(5) ?? shifted.getUTCMinutes()) === shifted.getUTCMinutes() && (field(6) ?? shifted.getUTCSeconds()) === shifted.getUTCSeconds();
}
var pluralRulesCache = /* @__PURE__ */ new Map();
function getPluralRules(locale) {
  let rules = pluralRulesCache.get(locale);
  if (!rules) {
    rules = new Intl.PluralRules(locale);
    pluralRulesCache.set(locale, rules);
  }
  return rules;
}
var isTruthy = (value) => !!value;
function executeAnd(values, truthy = isTruthy) {
  if (!Array.isArray(values) || values.length < 2) {
    throw new A2uiExpressionError("and requires at least 2 values", "and");
  }
  return values.every(truthy);
}
function executeOr(values, truthy = isTruthy) {
  if (!Array.isArray(values) || values.length < 2) {
    throw new A2uiExpressionError("or requires at least 2 values", "or");
  }
  return values.some(truthy);
}
function executeNot(value, truthy = isTruthy) {
  return !truthy(value);
}
function adaptAstPartForV10(part) {
  if (typeof part !== "object" || part === null || Array.isArray(part)) {
    return part;
  }
  if ("path" in part && typeof part.path === "string" && !("@path" in part)) {
    return { "@path": part.path };
  }
  if ("call" in part && typeof part.call === "string" && !("@call" in part)) {
    const args = {};
    if (part.args && typeof part.args === "object") {
      for (const [k2, v3] of Object.entries(part.args)) {
        args[k2] = adaptAstPartForV10(v3);
      }
    }
    return { "@call": part.call, args, returnType: part.returnType };
  }
  return part;
}
function executeFormatString(template, context) {
  const parser2 = new ExpressionParser();
  const parts = parser2.parse(template);
  if (parts.length === 0)
    return "";
  const atLeastV10 = isAtLeastVersion(context.surface?.protocolVersion ?? context.surface?.defaultCatalog?.protocolVersion, "1.0");
  const dynamicParts = parts.map((part) => {
    if (typeof part !== "object" || part === null || Array.isArray(part)) {
      return part;
    }
    const adapted = atLeastV10 ? adaptAstPartForV10(part) : part;
    return context.resolveSignal(adapted);
  });
  return computed(() => {
    return dynamicParts.map((p5) => coerceToString(isSignal(p5) ? getValue(p5) : p5)).join("");
  });
}
function executeFormatNumber(value, decimals, grouping, locale = DEFAULT_LOCALE) {
  if (isNaN(value))
    return "";
  return getNumberFormat(locale, decimals, grouping).format(value);
}
function executeFormatCurrency(value, currencyInput, decimals = 2, grouping, locale = DEFAULT_LOCALE) {
  if (isNaN(value))
    return "";
  const currency = String(currencyInput).toUpperCase();
  try {
    return getCurrencyFormat(locale, currency, decimals, grouping).format(value);
  } catch {
    return getCurrencyCodeFormat(locale, decimals, grouping).formatToParts(value).map((part) => part.type === "currency" ? currency : part.value).join("");
  }
}
function executeFormatDate(value, formatPattern = "yyyy-MM-dd", locale = DEFAULT_LOCALE) {
  if (!value)
    return "";
  const parsed = parseTimestamp(String(value));
  if (!parsed)
    return "";
  const { shifted, instant } = parsed;
  const pattern = formatPattern || "yyyy-MM-dd";
  if (pattern === "ISO")
    return instant.toISOString();
  const names = getDateNames(locale);
  const weekdayIndex = (shifted.getUTCDay() + 6) % 7;
  const hours = shifted.getUTCHours();
  const hours12 = hours % 12 || 12;
  return pattern.replace(DATE_TOKENS, (token) => {
    switch (token) {
      case "yyyy":
        return String(shifted.getUTCFullYear());
      case "yy":
        return String(shifted.getUTCFullYear()).slice(-2);
      case "MMMM":
        return names.monthsLong[shifted.getUTCMonth()];
      case "MMM":
        return names.monthsShort[shifted.getUTCMonth()];
      case "MM":
        return String(shifted.getUTCMonth() + 1).padStart(2, "0");
      case "M":
        return String(shifted.getUTCMonth() + 1);
      case "EEEE":
        return names.weekdaysLong[weekdayIndex];
      case "E":
        return names.weekdaysShort[weekdayIndex];
      case "dd":
        return String(shifted.getUTCDate()).padStart(2, "0");
      case "d":
        return String(shifted.getUTCDate());
      case "HH":
        return String(hours).padStart(2, "0");
      case "H":
        return String(hours);
      case "hh":
        return String(hours12).padStart(2, "0");
      case "h":
        return String(hours12);
      case "mm":
        return String(shifted.getUTCMinutes()).padStart(2, "0");
      case "ss":
        return String(shifted.getUTCSeconds()).padStart(2, "0");
      case "a":
        return hours < 12 ? "AM" : "PM";
      default:
        return token;
    }
  });
}
function executePluralize(value, forms, locale = DEFAULT_LOCALE) {
  let category;
  if (value === 0 && forms["zero"] !== void 0)
    category = "zero";
  else if (value === 1 && forms["one"] !== void 0)
    category = "one";
  else if (value === 2 && forms["two"] !== void 0)
    category = "two";
  else
    category = getPluralRules(locale).select(value);
  return String(forms[category] ?? forms["other"] ?? "");
}
function executeOpenUrl(urlInput) {
  const target = typeof urlInput === "string" ? urlInput : void 0;
  if (!target || typeof window === "undefined" || typeof window["open"] !== "function")
    return;
  const baseHref = typeof window !== "undefined" && typeof window.location !== "undefined" && window.location.href ? window.location.href : void 0;
  let url;
  try {
    url = baseHref ? new URL(target, baseHref) : new URL(target);
  } catch (e7) {
    throw new A2uiExpressionError(`Invalid URL specified: ${target}`, "openUrl", e7);
  }
  if (url.protocol !== "https:" && url.protocol !== "http:" && url.protocol !== "mailto:" && url.protocol !== "tel:") {
    throw new A2uiExpressionError(`Unsupported URL scheme: ${url.protocol}`, "openUrl");
  }
  window.open(url.href, "_blank", "noopener,noreferrer");
}
function createAndImplementation(api, { truthy = isTruthy } = {}) {
  return createFunctionImplementation(api, (args) => executeAnd(args.values, truthy));
}
function createOrImplementation(api, { truthy = isTruthy } = {}) {
  return createFunctionImplementation(api, (args) => executeOr(args.values, truthy));
}
function createNotImplementation(api, { truthy = isTruthy } = {}) {
  return createFunctionImplementation(api, (args) => executeNot(args.value, truthy));
}
function createFormatStringImplementation(api) {
  return createFunctionImplementation(api, (args, context) => executeFormatString(args.value, context));
}
function createFormatNumberImplementation(api, locale) {
  const resolvedLocale = resolveLocale(locale);
  return createFunctionImplementation(api, (args) => executeFormatNumber(args.value, args.decimals, args.grouping, resolvedLocale));
}
function createFormatCurrencyImplementation(api, locale) {
  const resolvedLocale = resolveLocale(locale);
  return createFunctionImplementation(api, (args) => executeFormatCurrency(args.value, args.currency, args.decimals ?? 2, args.grouping, resolvedLocale));
}
function createFormatDateImplementation(api, locale) {
  const resolvedLocale = resolveLocale(locale);
  return createFunctionImplementation(api, (args) => executeFormatDate(args.value, args.format, resolvedLocale));
}
function createPluralizeImplementation(api, locale) {
  const resolvedLocale = resolveLocale(locale);
  return createFunctionImplementation(api, (args) => executePluralize(args.value, args, resolvedLocale));
}
function createOpenUrlImplementation(api) {
  return createFunctionImplementation(api, (args) => executeOpenUrl(args.url));
}

// node_modules/@a2ui/web_core/src/v0_9/basic_catalog/functions/basic_functions.js
var AddImplementation = createFunctionImplementation(AddApi, (args) => args.a + args.b);
var SubtractImplementation = createFunctionImplementation(SubtractApi, (args) => args.a - args.b);
var MultiplyImplementation = createFunctionImplementation(MultiplyApi, (args) => args.a * args.b);
var DivideImplementation = createFunctionImplementation(DivideApi, (args) => {
  const numA = Number(args.a);
  const numB = Number(args.b);
  if (Number.isNaN(numA) || Number.isNaN(numB)) {
    return NaN;
  }
  if (numB === 0) {
    return Infinity;
  }
  return numA / numB;
});
var EqualsImplementation = createFunctionImplementation(EqualsApi, (args) => args.a === args.b);
var NotEqualsImplementation = createFunctionImplementation(NotEqualsApi, (args) => args.a !== args.b);
var GreaterThanImplementation = createFunctionImplementation(GreaterThanApi, (args) => args.a > args.b);
var LessThanImplementation = createFunctionImplementation(LessThanApi, (args) => args.a < args.b);
var ContainsImplementation = createFunctionImplementation(ContainsApi, (args) => args.string.includes(args.substring));
var StartsWithImplementation = createFunctionImplementation(StartsWithApi, (args) => args.string.startsWith(args.prefix));
var EndsWithImplementation = createFunctionImplementation(EndsWithApi, (args) => args.string.endsWith(args.suffix));
var LEGACY_V09_FUNCTIONS = [
  AddImplementation,
  SubtractImplementation,
  MultiplyImplementation,
  DivideImplementation,
  EqualsImplementation,
  NotEqualsImplementation,
  GreaterThanImplementation,
  LessThanImplementation,
  ContainsImplementation,
  StartsWithImplementation,
  EndsWithImplementation
];
var AndImplementation = createAndImplementation(AndApi);
var OrImplementation = createOrImplementation(OrApi);
var NotImplementation = createNotImplementation(NotApi);
var RequiredImplementation = createFunctionImplementation(RequiredApi, (args) => validateRequired(args.value).valid);
var RegexImplementation = createFunctionImplementation(RegexApi, (args) => validateRegex(args.value, args.pattern).valid);
var LengthImplementation = createFunctionImplementation(LengthApi, (args) => validateLength(args.value, args.min, args.max).valid);
var NumericImplementation = createFunctionImplementation(NumericApi, (args) => validateNumeric(args.value, args.min, args.max).valid);
var EmailImplementation = createFunctionImplementation(EmailApi, (args) => validateEmail(args.value).valid);
var FormatStringImplementation = createFormatStringImplementation(FormatStringApi);
function createFormatNumberImplementation2(locale) {
  return createFormatNumberImplementation(FormatNumberApi, locale);
}
var FormatNumberImplementation = createFormatNumberImplementation2();
function createFormatCurrencyImplementation2(locale) {
  return createFormatCurrencyImplementation(FormatCurrencyApi, locale);
}
var FormatCurrencyImplementation = createFormatCurrencyImplementation2();
function createFormatDateImplementation2(locale) {
  return createFormatDateImplementation(FormatDateApi, locale);
}
var FormatDateImplementation = createFormatDateImplementation2();
function createPluralizeImplementation2(locale) {
  return createPluralizeImplementation(PluralizeApi, locale);
}
var PluralizeImplementation = createPluralizeImplementation2();
var OpenUrlImplementation = createOpenUrlImplementation(OpenUrlApi);
function createV09SpecFunctions(options) {
  const locale = options?.locale;
  return [
    RequiredImplementation,
    RegexImplementation,
    LengthImplementation,
    NumericImplementation,
    EmailImplementation,
    FormatStringImplementation,
    createFormatNumberImplementation2(locale),
    createFormatCurrencyImplementation2(locale),
    createFormatDateImplementation2(locale),
    createPluralizeImplementation2(locale),
    OpenUrlImplementation,
    AndImplementation,
    OrImplementation,
    NotImplementation
  ];
}
var V09_SPEC_FUNCTIONS = createV09SpecFunctions();
function createBasicCatalogFunctions(options) {
  return [...LEGACY_V09_FUNCTIONS, ...createV09SpecFunctions(options)];
}
var BASIC_FUNCTIONS = createBasicCatalogFunctions();

// node_modules/@a2ui/web_core/src/resolution/component-context.js
var ComponentContext = class {
  /**
   * Initializes a new `ComponentContext` instance.
   *
   * @param surface The surface model the component belongs to.
   * @param componentId The ID of the component.
   * @param dataModelBasePath The base path for data model access (default: '/').
   * @param parentDataContext Optional parent DataContext in the component tree.
   * @param index Optional explicit collection iteration index.
   */
  constructor(surface, componentId, dataModelBasePath = "/", parentDataContext, index) {
    const model = surface.componentsModel.get(componentId);
    if (!model) {
      throw new A2uiStateError(`Component not found: ${componentId}`);
    }
    this.componentModel = model;
    this.surfaceComponents = surface.componentsModel;
    this.theme = surface.theme;
    this.dataContext = new DataContext(surface, dataModelBasePath, index, parentDataContext);
    this._actionDispatcher = (action, metadata) => surface.dispatchAction(action, this.componentModel.id, metadata);
  }
  /**
   * Dispatches an action from the component.
   *
   * @param action The action to dispatch.
   * @param metadata Optional metadata envelope to attach to the outbound action.
   * @returns A promise that resolves when action dispatching completes.
   */
  dispatchAction(action, metadata) {
    return this._actionDispatcher(action, metadata);
  }
};

// node_modules/@a2ui/web_core/src/resolution/generic-binder.js
var behaviorCache = /* @__PURE__ */ new WeakMap();
function scrapeSchemaBehavior(schema) {
  const cached = behaviorCache.get(schema);
  if (cached) {
    return cached;
  }
  const behavior = getFieldBehavior(schema);
  const result = behavior.type === "OBJECT" && behavior.shape && !("accessibility" in behavior.shape) ? {
    ...behavior,
    shape: {
      ...behavior.shape,
      accessibility: getFieldBehavior(AccessibilityAttributesSchema)
    }
  } : behavior;
  behaviorCache.set(schema, result);
  return result;
}
function unwrapZodSchema(type) {
  let current = type;
  while (current) {
    const typeName = current._def?.typeName;
    if (typeName === "ZodOptional" || typeName === "ZodNullable" || typeName === "ZodDefault" || typeName === "ZodReadonly") {
      current = current._def.innerType;
    } else if (typeName === "ZodEffects") {
      current = current._def.schema;
    } else if (typeName === "ZodBranded") {
      current = current._def.type;
    } else if (typeName === "ZodLazy") {
      current = current._def.getter();
    } else {
      break;
    }
  }
  return current;
}
function getRefDefName(type) {
  let current = type;
  let desc = "";
  while (current) {
    desc = current.description ?? current._def?.description ?? "";
    if (desc)
      break;
    const typeName = current._def?.typeName;
    if (typeName === "ZodOptional" || typeName === "ZodNullable" || typeName === "ZodDefault" || typeName === "ZodReadonly") {
      current = current._def.innerType;
    } else if (typeName === "ZodEffects") {
      current = current._def.schema;
    } else if (typeName === "ZodBranded") {
      current = current._def.type;
    } else if (typeName === "ZodLazy") {
      current = current._def.getter();
    } else {
      break;
    }
  }
  if (!desc || !desc.startsWith("REF:"))
    return "";
  const cleanDesc = desc.slice(4);
  return extractRefDefName(cleanDesc.split("|")[0]);
}
function isCheckableField(type) {
  const defName = getRefDefName(type);
  if (defName === "Checkable" || defName === "CheckRule") {
    return true;
  }
  const current = unwrapZodSchema(type);
  if (current?._def?.typeName === "ZodArray") {
    const elem = current._def.type;
    const elemDefName = getRefDefName(elem);
    if (elemDefName === "CheckRule") {
      return true;
    }
  }
  return false;
}
function isActionOption(option) {
  if (getRefDefName(option) === "Action")
    return true;
  const current = unwrapZodSchema(option);
  const def = current._def;
  return def?.typeName === "ZodObject" && Boolean(def.shape?.().event);
}
function isDynamicOption(option) {
  const refDef = getRefDefName(option);
  if (refDef === "DataBinding" || refDef === "FunctionCall" || refDef.startsWith("Dynamic")) {
    return true;
  }
  const current = unwrapZodSchema(option);
  const def = current._def;
  if (def?.typeName !== "ZodObject")
    return false;
  const shape = def.shape?.() || {};
  const hasComponentId = Object.values(shape).some((prop) => getRefDefName(prop) === "ComponentId");
  return (Boolean(shape["@path"]) || Boolean(shape.path) || Boolean(shape["@call"]) || Boolean(shape.call)) && !hasComponentId;
}
function isChildListOption(option) {
  if (childRefKindOf(option) === "child-list" || getRefDefName(option) === "ChildList") {
    return true;
  }
  const current = unwrapZodSchema(option);
  const def = current._def;
  if (def?.typeName !== "ZodObject")
    return false;
  const shape = def.shape?.() || {};
  return Object.values(shape).some((prop) => getRefDefName(prop) === "ComponentId");
}
function isDynamicDef(defName, typeName) {
  return (defName === "DataBinding" || defName === "FunctionCall" || defName.startsWith("Dynamic")) && typeName !== "ZodObject" && typeName !== "ZodArray";
}
function matchUnionBehavior(options) {
  if (options.some(isActionOption))
    return { type: "ACTION" };
  if (options.some(isDynamicOption))
    return { type: "DYNAMIC" };
  if (options.some(isChildListOption))
    return { type: "STRUCTURAL" };
  return void 0;
}
function scrapeObjectShape(objShape) {
  const shape = {};
  for (const [key, value] of Object.entries(objShape)) {
    shape[key] = getFieldBehavior(value);
  }
  return shape;
}
function getFieldBehavior(type) {
  const defName = getRefDefName(type);
  if (isCheckableField(type)) {
    return { type: "CHECKABLE" };
  }
  if (defName === "Action") {
    return { type: "ACTION" };
  }
  const current = unwrapZodSchema(type);
  if (childRefKindOf(current) === "child-list" || defName === "ChildList") {
    return { type: "STRUCTURAL" };
  }
  if (isDynamicDef(defName, current._def?.typeName)) {
    return { type: "DYNAMIC" };
  }
  if (current._def.typeName === "ZodUnion") {
    const unionBehavior = matchUnionBehavior(current._def.options);
    if (unionBehavior)
      return unionBehavior;
  }
  if (current._def.typeName === "ZodArray") {
    return {
      type: "ARRAY",
      element: getFieldBehavior(current._def.type)
    };
  }
  if (current._def.typeName === "ZodObject") {
    return {
      type: "OBJECT",
      shape: scrapeObjectShape(current._def.shape())
    };
  }
  return { type: "STATIC" };
}
var MAX_DYNAMIC_CHILD_LIST_SIZE = 1e4;
function getSafeChildList(value) {
  if (!Array.isArray(value)) {
    return [];
  }
  return value.length > MAX_DYNAMIC_CHILD_LIST_SIZE ? value.slice(0, MAX_DYNAMIC_CHILD_LIST_SIZE) : value;
}
var GenericBinder = class {
  /**
   * Creates a new binder for the given component context and schema.
   *
   * @param context Component context providing state and event dispatching.
   * @param schema Zod schema defining the component properties.
   */
  constructor(context, schema) {
    this.dataListeners = [];
    this.propsListeners = [];
    this.currentProps = {};
    this.isConnected = false;
    this.actionClosures = /* @__PURE__ */ new Map();
    this.context = context;
    this.behaviorTree = scrapeSchemaBehavior(schema);
    if (this.behaviorTree.type !== "OBJECT") {
      this.behaviorTree = { type: "OBJECT", shape: {} };
    }
    this.resolveInitialProps();
  }
  resolveInitialProps() {
    const props = this.context.componentModel.properties;
    const resolved = this.resolveAndBind(props, this.behaviorTree, [], true);
    this.currentProps = { ...this.currentProps, ...resolved || {} };
  }
  connect() {
    if (this.isConnected)
      return;
    this.isConnected = true;
    const sub = this.context.componentModel.onUpdated.subscribe(() => {
      this.rebuildAllBindings();
    });
    this.compUnsub = () => sub.unsubscribe();
    this.rebuildAllBindings();
  }
  rebuildAllBindings() {
    this.dataListeners.forEach((l5) => l5());
    this.dataListeners = [];
    const props = this.context.componentModel.properties;
    const resolved = this.resolveAndBind(props, this.behaviorTree, [], false);
    this.currentProps = { ...this.currentProps, ...resolved || {} };
    this.notify();
  }
  bindDynamicValue(value, path, isSync) {
    const bound = this.context.dataContext.subscribeDynamicValue(value, (newVal) => {
      this.updateDeepValue(path, newVal);
      this.notify();
    });
    if (!isSync) {
      this.dataListeners.push(() => bound.unsubscribe());
    } else {
      bound.unsubscribe();
    }
    return bound.value;
  }
  /**
   * Synchronously evaluates dynamic expressions within a nested object tree.
   *
   * Traverses objects and arrays, resolving JSON pointer paths and function calls
   * against the current data context without creating persistent reactive subscriptions.
   *
   * @param val Raw value, nested object, or dynamic expression to evaluate.
   * @param depth Current recursion depth, bounded by MAX_DYNAMIC_VALUE_DEPTH.
   * @returns Evaluated data structure with dynamic expressions resolved.
   */
  resolveDeepSync(val, depth = 0) {
    if (typeof val !== "object" || val === null)
      return val;
    if (depth > MAX_DYNAMIC_VALUE_DEPTH)
      return void 0;
    if ("path" in val || "call" in val || "@path" in val || "@call" in val) {
      return this.context.dataContext.resolveDynamicValue(val, depth);
    }
    if (Array.isArray(val))
      return val.map((item) => this.resolveDeepSync(item, depth + 1));
    const res = {};
    for (const [k2, v3] of Object.entries(val)) {
      res[k2] = this.resolveDeepSync(v3, depth + 1);
    }
    return res;
  }
  bindAction(value, path) {
    const cacheKey = path.join("/");
    const cached = this.actionClosures.get(cacheKey);
    if (cached && jsonEquals(cached.raw, value)) {
      return cached.closure;
    }
    const closure = async (options) => {
      if (!value || typeof value !== "object") {
        return;
      }
      const valObj = value;
      const isFunctionCall = Boolean(valObj.functionCall && typeof valObj.functionCall === "object") || "call" in valObj || "@call" in valObj;
      if (isFunctionCall) {
        await this.context.dataContext.resolveAction(value, {
          userActivation: true
        });
        return;
      }
      const resolved = this.context.dataContext.resolveAction(value, { userActivation: true });
      if (resolved === void 0) {
        return;
      }
      await this.context.dispatchAction(resolved, options?.metadata);
    };
    this.actionClosures.set(cacheKey, { raw: value, closure });
    return closure;
  }
  mapTemplateChildren(rawArray, templateComponentId, templatePath) {
    const arr = getSafeChildList(rawArray);
    const listContext = this.context.dataContext.nested(templatePath);
    return arr.map((_3, i7) => ({
      id: templateComponentId,
      basePath: listContext.nested(String(i7)).path
    }));
  }
  bindStructuralTemplate(value, path, isSync) {
    if (!value || typeof value !== "object" || Array.isArray(value)) {
      return value;
    }
    const templateObj = value;
    const templatePath = typeof templateObj.path === "string" ? templateObj.path : void 0;
    const templateComponentId = typeof templateObj.componentId === "string" ? templateObj.componentId : void 0;
    if (!templatePath || !templateComponentId) {
      return value;
    }
    const atLeastV10 = isAtLeastVersion(this.context.dataContext.surface?.protocolVersion ?? this.context.dataContext.surface?.defaultCatalog?.protocolVersion, "1.0");
    const binding = atLeastV10 ? { "@path": templatePath } : { path: templatePath };
    const bound = this.context.dataContext.subscribeDynamicValue(binding, (newVal) => {
      const resolvedChildren = this.mapTemplateChildren(newVal, templateComponentId, templatePath);
      this.updateDeepValue(path, resolvedChildren);
      this.notify();
    });
    if (!isSync) {
      this.dataListeners.push(() => bound.unsubscribe());
    } else {
      bound.unsubscribe();
    }
    return this.mapTemplateChildren(bound.value, templateComponentId, templatePath);
  }
  extractValidationResult(val, explicitRuleMessage, fallbackMessage = "Validation failed") {
    if (typeof val === "object" && val !== null && "valid" in val) {
      const rec = val;
      const customMessage = rec.message;
      const severity = rec.severity === "warning" || rec.severity === "info" ? rec.severity : "error";
      const code = typeof rec.code === "string" ? rec.code : void 0;
      const message = explicitRuleMessage !== void 0 ? explicitRuleMessage : customMessage !== void 0 && customMessage !== null ? String(customMessage) : fallbackMessage;
      return {
        valid: Boolean(rec.valid),
        message,
        ...code !== void 0 ? { code } : {},
        severity
      };
    }
    return {
      valid: Boolean(val),
      message: explicitRuleMessage ?? fallbackMessage,
      severity: "error"
    };
  }
  bindCheckable(value, path, isSync) {
    const rules = Array.isArray(value) ? value : [];
    const ruleResults = rules.map(() => ({
      valid: true,
      message: "",
      severity: "error"
    }));
    const parentPath = path.slice(0, -1);
    const applyValidationState = () => {
      const errors = ruleResults.filter((r8) => !r8.valid && r8.severity === "error").map((r8) => r8.message);
      const failedResults = ruleResults.filter((r8) => !r8.valid);
      this.updateDeepValue([...parentPath, "isValid"], errors.length === 0);
      this.updateDeepValue([...parentPath, "validationErrors"], errors);
      this.updateDeepValue([...parentPath, "validationResults"], failedResults);
    };
    const updateValidationState = () => {
      applyValidationState();
      this.notify();
    };
    rules.forEach((rule, index) => {
      const ruleObj = typeof rule === "object" && rule !== null ? rule : void 0;
      const condition = ruleObj && ruleObj.condition !== void 0 ? ruleObj.condition : rule;
      const ruleMessage = typeof ruleObj?.message === "string" ? ruleObj.message : void 0;
      const explicitMessage = this.isPathBoundCondition(condition) ? void 0 : ruleMessage;
      const fallbackMessage = ruleMessage ?? "Validation failed";
      ruleResults[index].message = fallbackMessage;
      const bound = this.context.dataContext.subscribeDynamicValue(condition, (newVal) => {
        ruleResults[index] = this.extractValidationResult(newVal, explicitMessage, fallbackMessage);
        updateValidationState();
      });
      if (!isSync) {
        this.dataListeners.push(() => bound.unsubscribe());
      } else {
        bound.unsubscribe();
      }
      ruleResults[index] = this.extractValidationResult(bound.value, explicitMessage, fallbackMessage);
    });
    applyValidationState();
    return value;
  }
  /** Whether a check condition is a data binding rather than a function call. */
  isPathBoundCondition(condition) {
    if (typeof condition !== "object" || condition === null)
      return false;
    const rec = condition;
    return this.context.dataContext.atLeastV10 ? typeof rec["@path"] === "string" && !("@call" in rec) : typeof rec["path"] === "string" && !("call" in rec);
  }
  bindObject(valObj, shape, path, isSync) {
    const result = {};
    for (const [k2, v3] of Object.entries(valObj)) {
      const childBehavior = shape[k2] || { type: "STATIC" };
      result[k2] = this.resolveAndBind(v3, childBehavior, [...path, k2], isSync);
    }
    for (const [k2, childBehavior] of Object.entries(shape)) {
      if (childBehavior.type === "DYNAMIC") {
        const setterName = `set${k2.charAt(0).toUpperCase() + k2.slice(1)}`;
        const rawPropValue = valObj[k2];
        result[setterName] = (newValue) => {
          if (rawPropValue && typeof rawPropValue === "object") {
            const rawObj = rawPropValue;
            const bindingKey = this.context.dataContext.atLeastV10 ? "@path" : "path";
            const pathVal = rawObj[bindingKey];
            if (typeof pathVal === "string" && (this.context.dataContext.atLeastV10 || !("componentId" in rawObj))) {
              this.context.dataContext.set(pathVal, newValue);
            }
          }
        };
      }
    }
    return result;
  }
  resolveAndBind(value, behavior, path, isSync) {
    if (value === void 0 || value === null)
      return value;
    switch (behavior.type) {
      case "DYNAMIC":
        return this.bindDynamicValue(value, path, isSync);
      case "ACTION":
        return this.bindAction(value, path);
      case "STRUCTURAL":
        return this.bindStructuralTemplate(value, path, isSync);
      case "CHECKABLE":
        return this.bindCheckable(value, path, isSync);
      case "STATIC":
        return value;
      case "ARRAY":
        if (!Array.isArray(value))
          return value;
        return value.map((item, index) => this.resolveAndBind(item, behavior.element, [...path, index.toString()], isSync));
      case "OBJECT":
        if (typeof value !== "object")
          return value;
        return this.bindObject(value, behavior.shape, path, isSync);
    }
  }
  updateDeepValue(path, newValue) {
    this.currentProps = this.cloneAndUpdate(this.currentProps, path, newValue);
  }
  cloneAndUpdate(obj, path, newValue) {
    if (path.length === 0)
      return newValue;
    const [key, ...rest] = path;
    if (Array.isArray(obj)) {
      const newArr = [...obj];
      newArr[Number(key)] = this.cloneAndUpdate(newArr[Number(key)], rest, newValue);
      return newArr;
    } else {
      const record = typeof obj === "object" && obj !== null ? obj : {};
      return {
        ...record,
        [key]: this.cloneAndUpdate(record[key], rest, newValue)
      };
    }
  }
  /**
   * Disposes all active data subscriptions and detaches component listeners.
   */
  dispose() {
    if (!this.isConnected)
      return;
    this.isConnected = false;
    this.dataListeners.forEach((l5) => l5());
    this.dataListeners = [];
    if (this.compUnsub) {
      this.compUnsub();
      this.compUnsub = void 0;
    }
  }
  notify() {
    this.propsListeners.forEach((l5) => l5(this.currentProps));
  }
  /**
   * Subscribes to prop change notifications.
   *
   * @param listener Callback invoked whenever resolved properties update.
   * @returns A subscription object to unsubscribe.
   */
  subscribe(listener) {
    if (this.propsListeners.length === 0) {
      this.connect();
    }
    this.propsListeners.push(listener);
    return {
      unsubscribe: () => {
        this.propsListeners = this.propsListeners.filter((l5) => l5 !== listener);
        if (this.propsListeners.length === 0) {
          this.dispose();
        }
      }
    };
  }
  /**
   * Current snapshot of resolved component properties.
   */
  get snapshot() {
    return this.currentProps;
  }
};
function jsonEquals(a4, b4) {
  if (Object.is(a4, b4))
    return true;
  if (typeof a4 !== "object" || typeof b4 !== "object" || a4 === null || b4 === null) {
    return false;
  }
  if (Array.isArray(a4) || Array.isArray(b4)) {
    if (!Array.isArray(a4) || !Array.isArray(b4) || a4.length !== b4.length) {
      return false;
    }
    return a4.every((item, index) => jsonEquals(item, b4[index]));
  }
  const aKeys = Object.keys(a4);
  const bKeys = Object.keys(b4);
  if (aKeys.length !== bKeys.length)
    return false;
  return aKeys.every((key) => Object.prototype.hasOwnProperty.call(b4, key) && jsonEquals(a4[key], b4[key]));
}

// node_modules/@lit/reactive-element/css-tag.js
var t2 = globalThis;
var e2 = t2.ShadowRoot && (void 0 === t2.ShadyCSS || t2.ShadyCSS.nativeShadow) && "adoptedStyleSheets" in Document.prototype && "replace" in CSSStyleSheet.prototype;
var s2 = /* @__PURE__ */ Symbol();
var o2 = /* @__PURE__ */ new WeakMap();
var n2 = class {
  constructor(t7, e7, o7) {
    if (this._$cssResult$ = true, o7 !== s2) throw Error("CSSResult is not constructable. Use `unsafeCSS` or `css` instead.");
    this.cssText = t7, this.t = e7;
  }
  get styleSheet() {
    let t7 = this.o;
    const s5 = this.t;
    if (e2 && void 0 === t7) {
      const e7 = void 0 !== s5 && 1 === s5.length;
      e7 && (t7 = o2.get(s5)), void 0 === t7 && ((this.o = t7 = new CSSStyleSheet()).replaceSync(this.cssText), e7 && o2.set(s5, t7));
    }
    return t7;
  }
  toString() {
    return this.cssText;
  }
};
var r2 = (t7) => new n2("string" == typeof t7 ? t7 : t7 + "", void 0, s2);
var i2 = (t7, ...e7) => {
  const o7 = 1 === t7.length ? t7[0] : e7.reduce((e8, s5, o8) => e8 + ((t8) => {
    if (true === t8._$cssResult$) return t8.cssText;
    if ("number" == typeof t8) return t8;
    throw Error("Value passed to 'css' function must be a 'css' function result: " + t8 + ". Use 'unsafeCSS' to pass non-literal values, but take care to ensure page security.");
  })(s5) + t7[o8 + 1], t7[0]);
  return new n2(o7, t7, s2);
};
var S2 = (s5, o7) => {
  if (e2) s5.adoptedStyleSheets = o7.map((t7) => t7 instanceof CSSStyleSheet ? t7 : t7.styleSheet);
  else for (const e7 of o7) {
    const o8 = document.createElement("style"), n6 = t2.litNonce;
    void 0 !== n6 && o8.setAttribute("nonce", n6), o8.textContent = e7.cssText, s5.appendChild(o8);
  }
};
var c2 = e2 ? (t7) => t7 : (t7) => t7 instanceof CSSStyleSheet ? ((t8) => {
  let e7 = "";
  for (const s5 of t8.cssRules) e7 += s5.cssText;
  return r2(e7);
})(t7) : t7;

// node_modules/@lit/reactive-element/reactive-element.js
var { is: i3, defineProperty: e3, getOwnPropertyDescriptor: h2, getOwnPropertyNames: r3, getOwnPropertySymbols: o3, getPrototypeOf: n3 } = Object;
var a2 = globalThis;
var c3 = a2.trustedTypes;
var l2 = c3 ? c3.emptyScript : "";
var p2 = a2.reactiveElementPolyfillSupport;
var d2 = (t7, s5) => t7;
var u2 = { toAttribute(t7, s5) {
  switch (s5) {
    case Boolean:
      t7 = t7 ? l2 : null;
      break;
    case Object:
    case Array:
      t7 = null == t7 ? t7 : JSON.stringify(t7);
  }
  return t7;
}, fromAttribute(t7, s5) {
  let i7 = t7;
  switch (s5) {
    case Boolean:
      i7 = null !== t7;
      break;
    case Number:
      i7 = null === t7 ? null : Number(t7);
      break;
    case Object:
    case Array:
      try {
        i7 = JSON.parse(t7);
      } catch (t8) {
        i7 = null;
      }
  }
  return i7;
} };
var f2 = (t7, s5) => !i3(t7, s5);
var b2 = { attribute: true, type: String, converter: u2, reflect: false, useDefault: false, hasChanged: f2 };
Symbol.metadata ??= /* @__PURE__ */ Symbol("metadata"), a2.litPropertyMetadata ??= /* @__PURE__ */ new WeakMap();
var y2 = class extends HTMLElement {
  static addInitializer(t7) {
    this._$Ei(), (this.l ??= []).push(t7);
  }
  static get observedAttributes() {
    return this.finalize(), this._$Eh && [...this._$Eh.keys()];
  }
  static createProperty(t7, s5 = b2) {
    if (s5.state && (s5.attribute = false), this._$Ei(), this.prototype.hasOwnProperty(t7) && ((s5 = Object.create(s5)).wrapped = true), this.elementProperties.set(t7, s5), !s5.noAccessor) {
      const i7 = /* @__PURE__ */ Symbol(), h4 = this.getPropertyDescriptor(t7, i7, s5);
      void 0 !== h4 && e3(this.prototype, t7, h4);
    }
  }
  static getPropertyDescriptor(t7, s5, i7) {
    const { get: e7, set: r8 } = h2(this.prototype, t7) ?? { get() {
      return this[s5];
    }, set(t8) {
      this[s5] = t8;
    } };
    return { get: e7, set(s6) {
      const h4 = e7?.call(this);
      r8?.call(this, s6), this.requestUpdate(t7, h4, i7);
    }, configurable: true, enumerable: true };
  }
  static getPropertyOptions(t7) {
    return this.elementProperties.get(t7) ?? b2;
  }
  static _$Ei() {
    if (this.hasOwnProperty(d2("elementProperties"))) return;
    const t7 = n3(this);
    t7.finalize(), void 0 !== t7.l && (this.l = [...t7.l]), this.elementProperties = new Map(t7.elementProperties);
  }
  static finalize() {
    if (this.hasOwnProperty(d2("finalized"))) return;
    if (this.finalized = true, this._$Ei(), this.hasOwnProperty(d2("properties"))) {
      const t8 = this.properties, s5 = [...r3(t8), ...o3(t8)];
      for (const i7 of s5) this.createProperty(i7, t8[i7]);
    }
    const t7 = this[Symbol.metadata];
    if (null !== t7) {
      const s5 = litPropertyMetadata.get(t7);
      if (void 0 !== s5) for (const [t8, i7] of s5) this.elementProperties.set(t8, i7);
    }
    this._$Eh = /* @__PURE__ */ new Map();
    for (const [t8, s5] of this.elementProperties) {
      const i7 = this._$Eu(t8, s5);
      void 0 !== i7 && this._$Eh.set(i7, t8);
    }
    this.elementStyles = this.finalizeStyles(this.styles);
  }
  static finalizeStyles(s5) {
    const i7 = [];
    if (Array.isArray(s5)) {
      const e7 = new Set(s5.flat(1 / 0).reverse());
      for (const s6 of e7) i7.unshift(c2(s6));
    } else void 0 !== s5 && i7.push(c2(s5));
    return i7;
  }
  static _$Eu(t7, s5) {
    const i7 = s5.attribute;
    return false === i7 ? void 0 : "string" == typeof i7 ? i7 : "string" == typeof t7 ? t7.toLowerCase() : void 0;
  }
  constructor() {
    super(), this._$Ep = void 0, this.isUpdatePending = false, this.hasUpdated = false, this._$Em = null, this._$Ev();
  }
  _$Ev() {
    this._$ES = new Promise((t7) => this.enableUpdating = t7), this._$AL = /* @__PURE__ */ new Map(), this._$E_(), this.requestUpdate(), this.constructor.l?.forEach((t7) => t7(this));
  }
  addController(t7) {
    (this._$EO ??= /* @__PURE__ */ new Set()).add(t7), void 0 !== this.renderRoot && this.isConnected && t7.hostConnected?.();
  }
  removeController(t7) {
    this._$EO?.delete(t7);
  }
  _$E_() {
    const t7 = /* @__PURE__ */ new Map(), s5 = this.constructor.elementProperties;
    for (const i7 of s5.keys()) this.hasOwnProperty(i7) && (t7.set(i7, this[i7]), delete this[i7]);
    t7.size > 0 && (this._$Ep = t7);
  }
  createRenderRoot() {
    const t7 = this.shadowRoot ?? this.attachShadow(this.constructor.shadowRootOptions);
    return S2(t7, this.constructor.elementStyles), t7;
  }
  connectedCallback() {
    this.renderRoot ??= this.createRenderRoot(), this.enableUpdating(true), this._$EO?.forEach((t7) => t7.hostConnected?.());
  }
  enableUpdating(t7) {
  }
  disconnectedCallback() {
    this._$EO?.forEach((t7) => t7.hostDisconnected?.());
  }
  attributeChangedCallback(t7, s5, i7) {
    this._$AK(t7, i7);
  }
  _$ET(t7, s5) {
    const i7 = this.constructor.elementProperties.get(t7), e7 = this.constructor._$Eu(t7, i7);
    if (void 0 !== e7 && true === i7.reflect) {
      const h4 = (void 0 !== i7.converter?.toAttribute ? i7.converter : u2).toAttribute(s5, i7.type);
      this._$Em = t7, null == h4 ? this.removeAttribute(e7) : this.setAttribute(e7, h4), this._$Em = null;
    }
  }
  _$AK(t7, s5) {
    const i7 = this.constructor, e7 = i7._$Eh.get(t7);
    if (void 0 !== e7 && this._$Em !== e7) {
      const t8 = i7.getPropertyOptions(e7), h4 = "function" == typeof t8.converter ? { fromAttribute: t8.converter } : void 0 !== t8.converter?.fromAttribute ? t8.converter : u2;
      this._$Em = e7;
      const r8 = h4.fromAttribute(s5, t8.type);
      this[e7] = r8 ?? this._$Ej?.get(e7) ?? r8, this._$Em = null;
    }
  }
  requestUpdate(t7, s5, i7, e7 = false, h4) {
    if (void 0 !== t7) {
      const r8 = this.constructor;
      if (false === e7 && (h4 = this[t7]), i7 ??= r8.getPropertyOptions(t7), !((i7.hasChanged ?? f2)(h4, s5) || i7.useDefault && i7.reflect && h4 === this._$Ej?.get(t7) && !this.hasAttribute(r8._$Eu(t7, i7)))) return;
      this.C(t7, s5, i7);
    }
    false === this.isUpdatePending && (this._$ES = this._$EP());
  }
  C(t7, s5, { useDefault: i7, reflect: e7, wrapped: h4 }, r8) {
    i7 && !(this._$Ej ??= /* @__PURE__ */ new Map()).has(t7) && (this._$Ej.set(t7, r8 ?? s5 ?? this[t7]), true !== h4 || void 0 !== r8) || (this._$AL.has(t7) || (this.hasUpdated || i7 || (s5 = void 0), this._$AL.set(t7, s5)), true === e7 && this._$Em !== t7 && (this._$Eq ??= /* @__PURE__ */ new Set()).add(t7));
  }
  async _$EP() {
    this.isUpdatePending = true;
    try {
      await this._$ES;
    } catch (t8) {
      Promise.reject(t8);
    }
    const t7 = this.scheduleUpdate();
    return null != t7 && await t7, !this.isUpdatePending;
  }
  scheduleUpdate() {
    return this.performUpdate();
  }
  performUpdate() {
    if (!this.isUpdatePending) return;
    if (!this.hasUpdated) {
      if (this.renderRoot ??= this.createRenderRoot(), this._$Ep) {
        for (const [t9, s6] of this._$Ep) this[t9] = s6;
        this._$Ep = void 0;
      }
      const t8 = this.constructor.elementProperties;
      if (t8.size > 0) for (const [s6, i7] of t8) {
        const { wrapped: t9 } = i7, e7 = this[s6];
        true !== t9 || this._$AL.has(s6) || void 0 === e7 || this.C(s6, void 0, i7, e7);
      }
    }
    let t7 = false;
    const s5 = this._$AL;
    try {
      t7 = this.shouldUpdate(s5), t7 ? (this.willUpdate(s5), this._$EO?.forEach((t8) => t8.hostUpdate?.()), this.update(s5)) : this._$EM();
    } catch (s6) {
      throw t7 = false, this._$EM(), s6;
    }
    t7 && this._$AE(s5);
  }
  willUpdate(t7) {
  }
  _$AE(t7) {
    this._$EO?.forEach((t8) => t8.hostUpdated?.()), this.hasUpdated || (this.hasUpdated = true, this.firstUpdated(t7)), this.updated(t7);
  }
  _$EM() {
    this._$AL = /* @__PURE__ */ new Map(), this.isUpdatePending = false;
  }
  get updateComplete() {
    return this.getUpdateComplete();
  }
  getUpdateComplete() {
    return this._$ES;
  }
  shouldUpdate(t7) {
    return true;
  }
  update(t7) {
    this._$Eq &&= this._$Eq.forEach((t8) => this._$ET(t8, this[t8])), this._$EM();
  }
  updated(t7) {
  }
  firstUpdated(t7) {
  }
};
y2.elementStyles = [], y2.shadowRootOptions = { mode: "open" }, y2[d2("elementProperties")] = /* @__PURE__ */ new Map(), y2[d2("finalized")] = /* @__PURE__ */ new Map(), p2?.({ ReactiveElement: y2 }), (a2.reactiveElementVersions ??= []).push("2.1.2");

// node_modules/lit-html/lit-html.js
var t3 = globalThis;
var i4 = (t7) => t7;
var s3 = t3.trustedTypes;
var e4 = s3 ? s3.createPolicy("lit-html", { createHTML: (t7) => t7 }) : void 0;
var h3 = "$lit$";
var o4 = `lit$${Math.random().toFixed(9).slice(2)}$`;
var n4 = "?" + o4;
var r4 = `<${n4}>`;
var l3 = document;
var c4 = () => l3.createComment("");
var a3 = (t7) => null === t7 || "object" != typeof t7 && "function" != typeof t7;
var u3 = Array.isArray;
var d3 = (t7) => u3(t7) || "function" == typeof t7?.[Symbol.iterator];
var f3 = "[ 	\n\f\r]";
var v2 = /<(?:(!--|\/[^a-zA-Z])|(\/?[a-zA-Z][^>\s]*)|(\/?$))/g;
var _2 = /-->/g;
var m2 = />/g;
var p3 = RegExp(`>|${f3}(?:([^\\s"'>=/]+)(${f3}*=${f3}*(?:[^ 	
\f\r"'\`<>=]|("|')|))|$)`, "g");
var g2 = /'/g;
var $ = /"/g;
var y3 = /^(?:script|style|textarea|title)$/i;
var x2 = (t7) => (i7, ...s5) => ({ _$litType$: t7, strings: i7, values: s5 });
var b3 = x2(1);
var w2 = x2(2);
var T = x2(3);
var E = /* @__PURE__ */ Symbol.for("lit-noChange");
var A = /* @__PURE__ */ Symbol.for("lit-nothing");
var C = /* @__PURE__ */ new WeakMap();
var P = l3.createTreeWalker(l3, 129);
function V(t7, i7) {
  if (!u3(t7) || !t7.hasOwnProperty("raw")) throw Error("invalid template strings array");
  return void 0 !== e4 ? e4.createHTML(i7) : i7;
}
var N = (t7, i7) => {
  const s5 = t7.length - 1, e7 = [];
  let n6, l5 = 2 === i7 ? "<svg>" : 3 === i7 ? "<math>" : "", c5 = v2;
  for (let i8 = 0; i8 < s5; i8++) {
    const s6 = t7[i8];
    let a4, u4, d4 = -1, f4 = 0;
    for (; f4 < s6.length && (c5.lastIndex = f4, u4 = c5.exec(s6), null !== u4); ) f4 = c5.lastIndex, c5 === v2 ? "!--" === u4[1] ? c5 = _2 : void 0 !== u4[1] ? c5 = m2 : void 0 !== u4[2] ? (y3.test(u4[2]) && (n6 = RegExp("</" + u4[2], "g")), c5 = p3) : void 0 !== u4[3] && (c5 = p3) : c5 === p3 ? ">" === u4[0] ? (c5 = n6 ?? v2, d4 = -1) : void 0 === u4[1] ? d4 = -2 : (d4 = c5.lastIndex - u4[2].length, a4 = u4[1], c5 = void 0 === u4[3] ? p3 : '"' === u4[3] ? $ : g2) : c5 === $ || c5 === g2 ? c5 = p3 : c5 === _2 || c5 === m2 ? c5 = v2 : (c5 = p3, n6 = void 0);
    const x3 = c5 === p3 && t7[i8 + 1].startsWith("/>") ? " " : "";
    l5 += c5 === v2 ? s6 + r4 : d4 >= 0 ? (e7.push(a4), s6.slice(0, d4) + h3 + s6.slice(d4) + o4 + x3) : s6 + o4 + (-2 === d4 ? i8 : x3);
  }
  return [V(t7, l5 + (t7[s5] || "<?>") + (2 === i7 ? "</svg>" : 3 === i7 ? "</math>" : "")), e7];
};
var S3 = class _S {
  constructor({ strings: t7, _$litType$: i7 }, e7) {
    let r8;
    this.parts = [];
    let l5 = 0, a4 = 0;
    const u4 = t7.length - 1, d4 = this.parts, [f4, v3] = N(t7, i7);
    if (this.el = _S.createElement(f4, e7), P.currentNode = this.el.content, 2 === i7 || 3 === i7) {
      const t8 = this.el.content.firstChild;
      t8.replaceWith(...t8.childNodes);
    }
    for (; null !== (r8 = P.nextNode()) && d4.length < u4; ) {
      if (1 === r8.nodeType) {
        if (r8.hasAttributes()) for (const t8 of r8.getAttributeNames()) if (t8.endsWith(h3)) {
          const i8 = v3[a4++], s5 = r8.getAttribute(t8).split(o4), e8 = /([.?@])?(.*)/.exec(i8);
          d4.push({ type: 1, index: l5, name: e8[2], strings: s5, ctor: "." === e8[1] ? I : "?" === e8[1] ? L : "@" === e8[1] ? z : H }), r8.removeAttribute(t8);
        } else t8.startsWith(o4) && (d4.push({ type: 6, index: l5 }), r8.removeAttribute(t8));
        if (y3.test(r8.tagName)) {
          const t8 = r8.textContent.split(o4), i8 = t8.length - 1;
          if (i8 > 0) {
            r8.textContent = s3 ? s3.emptyScript : "";
            for (let s5 = 0; s5 < i8; s5++) r8.append(t8[s5], c4()), P.nextNode(), d4.push({ type: 2, index: ++l5 });
            r8.append(t8[i8], c4());
          }
        }
      } else if (8 === r8.nodeType) if (r8.data === n4) d4.push({ type: 2, index: l5 });
      else {
        let t8 = -1;
        for (; -1 !== (t8 = r8.data.indexOf(o4, t8 + 1)); ) d4.push({ type: 7, index: l5 }), t8 += o4.length - 1;
      }
      l5++;
    }
  }
  static createElement(t7, i7) {
    const s5 = l3.createElement("template");
    return s5.innerHTML = t7, s5;
  }
};
function M(t7, i7, s5 = t7, e7) {
  if (i7 === E) return i7;
  let h4 = void 0 !== e7 ? s5._$Co?.[e7] : s5._$Cl;
  const o7 = a3(i7) ? void 0 : i7._$litDirective$;
  return h4?.constructor !== o7 && (h4?._$AO?.(false), void 0 === o7 ? h4 = void 0 : (h4 = new o7(t7), h4._$AT(t7, s5, e7)), void 0 !== e7 ? (s5._$Co ??= [])[e7] = h4 : s5._$Cl = h4), void 0 !== h4 && (i7 = M(t7, h4._$AS(t7, i7.values), h4, e7)), i7;
}
var R = class {
  constructor(t7, i7) {
    this._$AV = [], this._$AN = void 0, this._$AD = t7, this._$AM = i7;
  }
  get parentNode() {
    return this._$AM.parentNode;
  }
  get _$AU() {
    return this._$AM._$AU;
  }
  u(t7) {
    const { el: { content: i7 }, parts: s5 } = this._$AD, e7 = (t7?.creationScope ?? l3).importNode(i7, true);
    P.currentNode = e7;
    let h4 = P.nextNode(), o7 = 0, n6 = 0, r8 = s5[0];
    for (; void 0 !== r8; ) {
      if (o7 === r8.index) {
        let i8;
        2 === r8.type ? i8 = new k(h4, h4.nextSibling, this, t7) : 1 === r8.type ? i8 = new r8.ctor(h4, r8.name, r8.strings, this, t7) : 6 === r8.type && (i8 = new Z(h4, this, t7)), this._$AV.push(i8), r8 = s5[++n6];
      }
      o7 !== r8?.index && (h4 = P.nextNode(), o7++);
    }
    return P.currentNode = l3, e7;
  }
  p(t7) {
    let i7 = 0;
    for (const s5 of this._$AV) void 0 !== s5 && (void 0 !== s5.strings ? (s5._$AI(t7, s5, i7), i7 += s5.strings.length - 2) : s5._$AI(t7[i7])), i7++;
  }
};
var k = class _k {
  get _$AU() {
    return this._$AM?._$AU ?? this._$Cv;
  }
  constructor(t7, i7, s5, e7) {
    this.type = 2, this._$AH = A, this._$AN = void 0, this._$AA = t7, this._$AB = i7, this._$AM = s5, this.options = e7, this._$Cv = e7?.isConnected ?? true;
  }
  get parentNode() {
    let t7 = this._$AA.parentNode;
    const i7 = this._$AM;
    return void 0 !== i7 && 11 === t7?.nodeType && (t7 = i7.parentNode), t7;
  }
  get startNode() {
    return this._$AA;
  }
  get endNode() {
    return this._$AB;
  }
  _$AI(t7, i7 = this) {
    t7 = M(this, t7, i7), a3(t7) ? t7 === A || null == t7 || "" === t7 ? (this._$AH !== A && this._$AR(), this._$AH = A) : t7 !== this._$AH && t7 !== E && this._(t7) : void 0 !== t7._$litType$ ? this.$(t7) : void 0 !== t7.nodeType ? this.T(t7) : d3(t7) ? this.k(t7) : this._(t7);
  }
  O(t7) {
    return this._$AA.parentNode.insertBefore(t7, this._$AB);
  }
  T(t7) {
    this._$AH !== t7 && (this._$AR(), this._$AH = this.O(t7));
  }
  _(t7) {
    this._$AH !== A && a3(this._$AH) ? this._$AA.nextSibling.data = t7 : this.T(l3.createTextNode(t7)), this._$AH = t7;
  }
  $(t7) {
    const { values: i7, _$litType$: s5 } = t7, e7 = "number" == typeof s5 ? this._$AC(t7) : (void 0 === s5.el && (s5.el = S3.createElement(V(s5.h, s5.h[0]), this.options)), s5);
    if (this._$AH?._$AD === e7) this._$AH.p(i7);
    else {
      const t8 = new R(e7, this), s6 = t8.u(this.options);
      t8.p(i7), this.T(s6), this._$AH = t8;
    }
  }
  _$AC(t7) {
    let i7 = C.get(t7.strings);
    return void 0 === i7 && C.set(t7.strings, i7 = new S3(t7)), i7;
  }
  k(t7) {
    u3(this._$AH) || (this._$AH = [], this._$AR());
    const i7 = this._$AH;
    let s5, e7 = 0;
    for (const h4 of t7) e7 === i7.length ? i7.push(s5 = new _k(this.O(c4()), this.O(c4()), this, this.options)) : s5 = i7[e7], s5._$AI(h4), e7++;
    e7 < i7.length && (this._$AR(s5 && s5._$AB.nextSibling, e7), i7.length = e7);
  }
  _$AR(t7 = this._$AA.nextSibling, s5) {
    for (this._$AP?.(false, true, s5); t7 !== this._$AB; ) {
      const s6 = i4(t7).nextSibling;
      i4(t7).remove(), t7 = s6;
    }
  }
  setConnected(t7) {
    void 0 === this._$AM && (this._$Cv = t7, this._$AP?.(t7));
  }
};
var H = class {
  get tagName() {
    return this.element.tagName;
  }
  get _$AU() {
    return this._$AM._$AU;
  }
  constructor(t7, i7, s5, e7, h4) {
    this.type = 1, this._$AH = A, this._$AN = void 0, this.element = t7, this.name = i7, this._$AM = e7, this.options = h4, s5.length > 2 || "" !== s5[0] || "" !== s5[1] ? (this._$AH = Array(s5.length - 1).fill(new String()), this.strings = s5) : this._$AH = A;
  }
  _$AI(t7, i7 = this, s5, e7) {
    const h4 = this.strings;
    let o7 = false;
    if (void 0 === h4) t7 = M(this, t7, i7, 0), o7 = !a3(t7) || t7 !== this._$AH && t7 !== E, o7 && (this._$AH = t7);
    else {
      const e8 = t7;
      let n6, r8;
      for (t7 = h4[0], n6 = 0; n6 < h4.length - 1; n6++) r8 = M(this, e8[s5 + n6], i7, n6), r8 === E && (r8 = this._$AH[n6]), o7 ||= !a3(r8) || r8 !== this._$AH[n6], r8 === A ? t7 = A : t7 !== A && (t7 += (r8 ?? "") + h4[n6 + 1]), this._$AH[n6] = r8;
    }
    o7 && !e7 && this.j(t7);
  }
  j(t7) {
    t7 === A ? this.element.removeAttribute(this.name) : this.element.setAttribute(this.name, t7 ?? "");
  }
};
var I = class extends H {
  constructor() {
    super(...arguments), this.type = 3;
  }
  j(t7) {
    this.element[this.name] = t7 === A ? void 0 : t7;
  }
};
var L = class extends H {
  constructor() {
    super(...arguments), this.type = 4;
  }
  j(t7) {
    this.element.toggleAttribute(this.name, !!t7 && t7 !== A);
  }
};
var z = class extends H {
  constructor(t7, i7, s5, e7, h4) {
    super(t7, i7, s5, e7, h4), this.type = 5;
  }
  _$AI(t7, i7 = this) {
    if ((t7 = M(this, t7, i7, 0) ?? A) === E) return;
    const s5 = this._$AH, e7 = t7 === A && s5 !== A || t7.capture !== s5.capture || t7.once !== s5.once || t7.passive !== s5.passive, h4 = t7 !== A && (s5 === A || e7);
    e7 && this.element.removeEventListener(this.name, this, s5), h4 && this.element.addEventListener(this.name, this, t7), this._$AH = t7;
  }
  handleEvent(t7) {
    "function" == typeof this._$AH ? this._$AH.call(this.options?.host ?? this.element, t7) : this._$AH.handleEvent(t7);
  }
};
var Z = class {
  constructor(t7, i7, s5) {
    this.element = t7, this.type = 6, this._$AN = void 0, this._$AM = i7, this.options = s5;
  }
  get _$AU() {
    return this._$AM._$AU;
  }
  _$AI(t7) {
    M(this, t7);
  }
};
var j2 = { M: h3, P: o4, A: n4, C: 1, L: N, R, D: d3, V: M, I: k, H, N: L, U: z, B: I, F: Z };
var B = t3.litHtmlPolyfillSupport;
B?.(S3, k), (t3.litHtmlVersions ??= []).push("3.3.3");
var D = (t7, i7, s5) => {
  const e7 = s5?.renderBefore ?? i7;
  let h4 = e7._$litPart$;
  if (void 0 === h4) {
    const t8 = s5?.renderBefore ?? null;
    e7._$litPart$ = h4 = new k(i7.insertBefore(c4(), t8), t8, void 0, s5 ?? {});
  }
  return h4._$AI(t7), h4;
};

// node_modules/lit-element/lit-element.js
var s4 = globalThis;
var i5 = class extends y2 {
  constructor() {
    super(...arguments), this.renderOptions = { host: this }, this._$Do = void 0;
  }
  createRenderRoot() {
    const t7 = super.createRenderRoot();
    return this.renderOptions.renderBefore ??= t7.firstChild, t7;
  }
  update(t7) {
    const r8 = this.render();
    this.hasUpdated || (this.renderOptions.isConnected = this.isConnected), super.update(t7), this._$Do = D(r8, this.renderRoot, this.renderOptions);
  }
  connectedCallback() {
    super.connectedCallback(), this._$Do?.setConnected(true);
  }
  disconnectedCallback() {
    super.disconnectedCallback(), this._$Do?.setConnected(false);
  }
  render() {
    return E;
  }
};
i5._$litElement$ = true, i5["finalized"] = true, s4.litElementHydrateSupport?.({ LitElement: i5 });
var o5 = s4.litElementPolyfillSupport;
o5?.({ LitElement: i5 });
(s4.litElementVersions ??= []).push("4.2.2");

// node_modules/@lit/reactive-element/decorators/custom-element.js
var t4 = (t7) => (e7, o7) => {
  void 0 !== o7 ? o7.addInitializer(() => {
    customElements.define(t7, e7);
  }) : customElements.define(t7, e7);
};

// node_modules/@lit/reactive-element/decorators/property.js
var o6 = { attribute: true, type: String, converter: u2, reflect: false, hasChanged: f2 };
var r5 = (t7 = o6, e7, r8) => {
  const { kind: n6, metadata: i7 } = r8;
  let s5 = globalThis.litPropertyMetadata.get(i7);
  if (void 0 === s5 && globalThis.litPropertyMetadata.set(i7, s5 = /* @__PURE__ */ new Map()), "setter" === n6 && ((t7 = Object.create(t7)).wrapped = true), s5.set(r8.name, t7), "accessor" === n6) {
    const { name: o7 } = r8;
    return { set(r9) {
      const n7 = e7.get.call(this);
      e7.set.call(this, r9), this.requestUpdate(o7, n7, t7, true, r9);
    }, init(e8) {
      return void 0 !== e8 && this.C(o7, void 0, t7, e8), e8;
    } };
  }
  if ("setter" === n6) {
    const { name: o7 } = r8;
    return function(r9) {
      const n7 = this[o7];
      e7.call(this, r9), this.requestUpdate(o7, n7, t7, true, r9);
    };
  }
  throw Error("Unsupported decorator location: " + n6);
};
function n5(t7) {
  return (e7, o7) => "object" == typeof o7 ? r5(t7, e7, o7) : ((t8, e8, o8) => {
    const r8 = e8.hasOwnProperty(o8);
    return e8.constructor.createProperty(o8, t8), r8 ? Object.getOwnPropertyDescriptor(e8, o8) : void 0;
  })(t7, e7, o7);
}

// node_modules/@lit/reactive-element/decorators/state.js
function r6(r8) {
  return n5({ ...r8, state: true, attribute: false });
}

// node_modules/@a2ui/web_core/src/resolution/resolved-binding.js
var ResolvedBinding = class {
  /**
   * Creates a new `ResolvedBinding` instance.
   *
   * @param value The resolved snapshot value.
   */
  constructor(value) {
    this.value = value;
  }
};
var WritableBinding = class extends ResolvedBinding {
  /**
   * Creates a new `WritableBinding` instance.
   *
   * @param value Current snapshot value.
   * @param set Setter callback to update the underlying model.
   * @param path Authored data path.
   */
  constructor(value, set, path) {
    super(value);
    this.set = set;
    this.path = path;
  }
};
function isWritable(binding) {
  return binding instanceof WritableBinding;
}
function sameBinding(a4, b4) {
  if (isWritable(a4) !== isWritable(b4)) {
    return false;
  }
  if (isWritable(a4) && isWritable(b4) && a4.path !== b4.path) {
    return false;
  }
  return valueEquals(a4.value, b4.value);
}
function valueEquals(a4, b4) {
  if (Object.is(a4, b4)) {
    return true;
  }
  if (Array.isArray(a4) && Array.isArray(b4)) {
    return a4.length === b4.length && a4.every((item, i7) => valueEquals(item, b4[i7]));
  }
  if (isPlainObject5(a4) && isPlainObject5(b4)) {
    const aKeys = Object.keys(a4);
    const bKeys = Object.keys(b4);
    return aKeys.length === bKeys.length && aKeys.every((key) => key in b4 && valueEquals(a4[key], b4[key]));
  }
  return false;
}
function isPlainObject5(value) {
  if (typeof value !== "object" || value === null) {
    return false;
  }
  const proto = Object.getPrototypeOf(value);
  return proto === Object.prototype || proto === null;
}

// node_modules/@a2ui/web_core/src/resolution/node-id.js
var COUNTER_KEY = /* @__PURE__ */ Symbol.for("a2ui.nodeId");
function nextNodeId() {
  const global = globalThis;
  const next = (global[COUNTER_KEY] ?? 0) + 1;
  global[COUNTER_KEY] = next;
  return `a2ui-n${next}`;
}

// node_modules/@a2ui/web_core/src/resolution/component-node.js
var PLACEHOLDER_TYPE = "Placeholder";
function isComponentNode(value) {
  return value instanceof MutableComponentNode;
}
var MutableComponentNode = class {
  /**
   * Creates a new `MutableComponentNode` instance.
   *
   * @param instanceId Unique instance identifier in the tree.
   * @param componentId The component ID from the payload.
   * @param type The component type name.
   * @param dataPath Data model scope path.
   * @param initialProps Initial resolved properties.
   * @param impl Optional catalog component definition.
   * @param context The context the node is bound with; absent on placeholders.
   * @param state Initial resolution state.
   * @param metadata Optional read-only reactive metadata signal.
   */
  constructor(instanceId, componentId, type, dataPath, initialProps, impl, context, state = "resolved", metadata = computed(() => void 0)) {
    this.id = nextNodeId();
    this._onDestroyed = new EventEmitter();
    this.onDestroyed = this._onDestroyed;
    this.cleanups = [];
    this._disposed = false;
    this.instanceId = instanceId;
    this.componentId = componentId;
    this.type = type;
    this.dataPath = dataPath;
    this.props = signal(initialProps);
    this.impl = impl;
    this.context = context;
    this.state = state;
    this.metadata = metadata;
  }
  /** Whether this node has been disposed. */
  get disposed() {
    return this._disposed;
  }
  /** Whether this node is an unresolved placeholder. */
  get isPlaceholder() {
    return this.state !== "resolved";
  }
  /**
   * Registers teardown work to run when this node is disposed.
   *
   * @param cleanup Teardown callback.
   */
  addCleanup(cleanup) {
    this.cleanups.push(cleanup);
  }
  /**
   * Serializes the resolved tree for debugging and headless assertions.
   *
   * @returns Serialized JSON-compatible representation.
   */
  toJSON() {
    if (this.isPlaceholder) {
      return { id: this.componentId, type: this.type, state: this.state };
    }
    const serialized = {
      id: this.componentId,
      type: this.type
    };
    const props = peekValue(this.props);
    for (const [key, value] of Object.entries(props)) {
      serialized[key] = serializeValue(value);
    }
    const metadata = peekValue(this.metadata);
    if (metadata !== void 0) {
      serialized.metadata = metadata;
    }
    return serialized;
  }
  /**
   * Replaces resolved properties, emitting only when a shallow comparison detects a change.
   *
   * @param next Next property dictionary.
   */
  setProps(next) {
    if (this._disposed) {
      return;
    }
    const previous = peekValue(this.props);
    if (!shallowEqual(previous, next)) {
      setValue(this.props, next);
    }
  }
  /**
   * Tears down this node by running cleanups and firing destruction events.
   *
   * This operation is idempotent.
   */
  dispose() {
    if (this._disposed) {
      return;
    }
    this._disposed = true;
    for (const cleanup of this.cleanups) {
      try {
        cleanup();
      } catch (e7) {
        console.error(`ComponentNode cleanup error (${this.instanceId}):`, e7);
      }
    }
    this.cleanups = [];
    this._onDestroyed.emit();
    this._onDestroyed.dispose();
  }
};
function serializeValue(value) {
  if (isComponentNode(value)) {
    return value.toJSON();
  }
  if (value instanceof ResolvedBinding) {
    return serializeValue(value.value);
  }
  if (typeof value === "function") {
    return "<Action>";
  }
  if (Array.isArray(value)) {
    return value.map(serializeValue);
  }
  if (value && typeof value === "object") {
    const result = {};
    for (const [key, inner] of Object.entries(value)) {
      result[key] = serializeValue(inner);
    }
    return result;
  }
  return value;
}
function shallowEqual(a4, b4) {
  if (Object.is(a4, b4)) {
    return true;
  }
  const aKeys = Object.keys(a4);
  const bKeys = Object.keys(b4);
  if (aKeys.length !== bKeys.length) {
    return false;
  }
  for (const key of aKeys) {
    if (!Object.is(a4[key], b4[key])) {
      return false;
    }
  }
  return true;
}

// node_modules/lit-html/directive.js
var t5 = { ATTRIBUTE: 1, CHILD: 2, PROPERTY: 3, BOOLEAN_ATTRIBUTE: 4, EVENT: 5, ELEMENT: 6 };
var e6 = (t7) => (...e7) => ({ _$litDirective$: t7, values: e7 });
var i6 = class {
  constructor(t7) {
  }
  get _$AU() {
    return this._$AM._$AU;
  }
  _$AT(t7, e7, i7) {
    this._$Ct = t7, this._$AM = e7, this._$Ci = i7;
  }
  _$AS(t7, e7) {
    return this.update(t7, e7);
  }
  update(t7, e7) {
    return this.render(...e7);
  }
};

// node_modules/@a2ui/web_core/src/universal/is_web_component_implementation.js
function isWebComponentImplementation(api) {
  return typeof api === "object" && api !== null && "tagName" in api && typeof api.tagName === "string" && "element" in api && typeof api.element === "function";
}

// node_modules/@a2ui/web_core/src/universal/register_universal_element.js
var missingCustomElementsReported = false;
function registerUniversalElement(component) {
  if (typeof customElements === "undefined") {
    if (!missingCustomElementsReported) {
      missingCustomElementsReported = true;
      console.error(`Cannot register "${component.tagName}": \`customElements\` is not available in this environment, so universal components will not render.`);
    }
    return;
  }
  const existing = customElements.get(component.tagName);
  if (!existing) {
    customElements.define(component.tagName, component.element);
    return;
  }
  if (existing !== component.element) {
    throw new Error(`Custom element tag name collision for "${component.tagName}": attempted to register ${component.element.name || "anonymous constructor"}, but tag name is already registered with ${existing.name || "another constructor"}.`);
  }
}

// node_modules/@a2ui/web_core/src/universal/render-a2ui-node.js
var A2uiElementDirective = class extends i6 {
  render(tagName, context, node) {
    if (!this.element || this.tagName !== tagName) {
      this.tagName = tagName;
      this.element = document.createElement(tagName);
    }
    this.element.node = node;
    this.element.context = context;
    return this.element;
  }
};
var a2uiElement = e6(A2uiElementDirective);
function renderA2uiNode(source, catalog) {
  if (isComponentNode(source)) {
    return renderNode(source);
  }
  const type = source.componentModel.type;
  const implementation = catalog?.components.get(type);
  if (!implementation || !implementation.tagName) {
    console.warn(`Component implementation not found or missing tagName for type: ${type}`);
    return A;
  }
  if (isWebComponentImplementation(implementation) && typeof customElements !== "undefined" && !customElements.get(implementation.tagName)) {
    registerUniversalElement(implementation);
  }
  return a2uiElement(implementation.tagName, source);
}
function renderNode(node) {
  if (node.isPlaceholder || node.disposed || !node.context) {
    return A;
  }
  const implementation = node.impl;
  if (!implementation?.tagName) {
    console.warn(`Component implementation not found or missing tagName for type: ${node.type}`);
    return A;
  }
  if (isWebComponentImplementation(implementation)) {
    registerUniversalElement(implementation);
  }
  return a2uiElement(implementation.tagName, node.context, node);
}

// node_modules/@a2ui/web_core/src/universal/a2ui-controller.js
function isPlainObject6(value) {
  if (!value || typeof value !== "object") {
    return false;
  }
  const proto = Object.getPrototypeOf(value);
  return proto === Object.prototype || proto === null;
}
function toControllerValue(parent, value) {
  if (isComponentNode(value)) {
    return value.dataPath !== parent.dataPath ? { id: value.componentId, basePath: value.dataPath } : value.componentId;
  }
  if (value instanceof ResolvedBinding) {
    return toControllerValue(parent, value.value);
  }
  if (Array.isArray(value)) {
    return value.map((item) => toControllerValue(parent, item));
  }
  if (isPlainObject6(value)) {
    return nodePropsToControllerProps(parent, value);
  }
  return value;
}
function nodePropsToControllerProps(parent, props) {
  const result = {};
  for (const [key, inner] of Object.entries(props)) {
    if (inner instanceof ResolvedBinding) {
      result[key] = toControllerValue(parent, inner.value);
      result[`set${key.charAt(0).toUpperCase()}${key.slice(1)}`] = isWritable(inner) ? inner.set : () => {
      };
    } else {
      result[key] = toControllerValue(parent, inner);
    }
  }
  return result;
}
var A2uiController = class {
  /**
   * The current reactive properties of the A2UI component, matching the expected output schema.
   */
  get props() {
    return this._props;
  }
  /**
   * The current envelope metadata of the A2UI component, lazily subscribing
   * to metadata changes on first access so standard components that do not
   * read metadata skip metadata-only re-renders.
   */
  get metadata() {
    this.metadataSubscribed = true;
    if (this.host.isConnected) {
      this.ensureMetadataSubscription();
    }
    if (this.host.node) {
      return peekValue(this.host.node.metadata);
    }
    return this.host.context?.componentModel?.metadata;
  }
  /**
   * The current `extensions` map of the A2UI component's metadata envelope,
   * lazily subscribing to metadata changes on first access.
   */
  get extensions() {
    return this.metadata?.extensions;
  }
  /**
   * The current surface-level metadata envelope of the containing surface,
   * lazily subscribing to surface metadata changes on first access.
   */
  get surfaceMetadata() {
    this.surfaceMetadataSubscribed = true;
    if (this.host.isConnected) {
      this.ensureSurfaceMetadataSubscription();
    }
    const surface = this.host.node?.context?.dataContext.surface ?? this.host.context?.dataContext.surface;
    return surface?.metadata;
  }
  /**
   * The current surface-level `extensions` map of the containing surface,
   * lazily subscribing to surface metadata changes on first access.
   */
  get surfaceExtensions() {
    return this.surfaceMetadata?.extensions;
  }
  /**
   * Initializes the controller, binding it to the given Lit element and API schema.
   *
   * @param host The A2uiLitElement acting as the component host.
   * @param api The A2UI component API defining the schema for this element.
   */
  constructor(host, api) {
    this.host = host;
    this.metadataSubscribed = false;
    this.surfaceMetadataSubscribed = false;
    if (this.host.node) {
      this._props = nodePropsToControllerProps(this.host.node, peekValue(this.host.node.props));
    } else {
      this.binder = new GenericBinder(this.host.context, api.schema);
      this._props = this.binder.snapshot;
    }
    this.host.addController(this);
    if (this.host.isConnected) {
      this.hostConnected();
    }
  }
  /**
   * Subscribes to `host.node.props` (or `GenericBinder` when no node is set)
   * when the host connects, triggering `requestUpdate()` on changes.
   */
  hostConnected() {
    this.ensureMetadataSubscription();
    this.ensureSurfaceMetadataSubscription();
    if (this.host.node) {
      if (!this.nodeEffectCleanup) {
        const node = this.host.node;
        this.nodeEffectCleanup = effect(() => {
          this._props = nodePropsToControllerProps(node, getValue(node.props));
          this.host.requestUpdate();
        });
      }
      return;
    }
    if (!this.subscription && this.binder) {
      this.subscription = this.binder.subscribe((newProps) => {
        this._props = newProps;
        this.host.requestUpdate();
      });
    }
    if (!this.componentsSubscription) {
      this.componentsSubscription = this.host.context.dataContext.surface?.componentsModel?.onCreated.subscribe(() => {
        this.host.requestUpdate();
      });
    }
  }
  ensureMetadataSubscription() {
    if (!this.metadataSubscribed) {
      return;
    }
    if (this.host.node) {
      if (!this.metadataEffectCleanup) {
        const node = this.host.node;
        let isFirstRun = true;
        this.metadataEffectCleanup = effect(() => {
          getValue(node.metadata);
          if (isFirstRun) {
            isFirstRun = false;
            return;
          }
          this.host.requestUpdate();
        });
      }
      return;
    }
    if (!this.metadataSubscription && this.host.context?.componentModel) {
      this.metadataSubscription = this.host.context.componentModel.onMetadataChanged.subscribe(() => {
        this.host.requestUpdate();
      });
    }
  }
  ensureSurfaceMetadataSubscription() {
    if (!this.surfaceMetadataSubscribed) {
      return;
    }
    const surface = this.host.node?.context?.dataContext.surface ?? this.host.context?.dataContext.surface;
    if (!this.surfaceMetadataSubscription && surface) {
      this.surfaceMetadataSubscription = surface.onMetadataChanged.subscribe(() => {
        this.host.requestUpdate();
      });
    }
  }
  /**
   * Unsubscribes from `node.props` or `GenericBinder` updates when the host disconnects.
   */
  hostDisconnected() {
    this.nodeEffectCleanup?.();
    this.nodeEffectCleanup = void 0;
    this.metadataEffectCleanup?.();
    this.metadataEffectCleanup = void 0;
    this.metadataSubscription?.unsubscribe();
    this.metadataSubscription = void 0;
    this.surfaceMetadataSubscription?.unsubscribe();
    this.surfaceMetadataSubscription = void 0;
    this.subscription?.unsubscribe();
    this.subscription = void 0;
    this.componentsSubscription?.unsubscribe();
    this.componentsSubscription = void 0;
  }
  /**
   * Unsubscribes host listeners and disposes the underlying GenericBinder, if any.
   */
  dispose() {
    this.hostDisconnected();
    this.binder?.dispose();
  }
};

// node_modules/@a2ui/web_core/src/universal/a2ui-lit-element.js
var __decorate = function(decorators, target, key, desc) {
  var c5 = arguments.length, r8 = c5 < 3 ? target : desc === null ? desc = Object.getOwnPropertyDescriptor(target, key) : desc, d4;
  if (typeof Reflect === "object" && typeof Reflect.decorate === "function") r8 = Reflect.decorate(decorators, target, key, desc);
  else for (var i7 = decorators.length - 1; i7 >= 0; i7--) if (d4 = decorators[i7]) r8 = (c5 < 3 ? d4(r8) : c5 > 3 ? d4(target, key, r8) : d4(target, key)) || r8;
  return c5 > 3 && r8 && Object.defineProperty(target, key, r8), r8;
};
var A2uiLitElement = class extends i5 {
  /**
   * The reactive controller instance managing property bindings and state subscriptions.
   */
  get controller() {
    return this._controller;
  }
  /**
   * The component's current metadata envelope, lazily subscribing on first read.
   */
  get metadata() {
    return this._controller?.metadata;
  }
  /**
   * The component's current `extensions` map, lazily subscribing on first read.
   */
  get extensions() {
    return this._controller?.extensions;
  }
  /**
   * The containing surface's metadata envelope, lazily subscribing on first read.
   */
  get surfaceMetadata() {
    return this._controller?.surfaceMetadata;
  }
  /**
   * The containing surface's `extensions` map, lazily subscribing on first read.
   */
  get surfaceExtensions() {
    return this._controller?.surfaceExtensions;
  }
  /**
   * Adopts and scopes component CSS rules into the containing document or host shadow root.
   *
   * When an element opts into Light DOM (`createRenderRoot()` returning `this`), it has no
   * shadow root of its own, so this method locates the nearest enclosing `Document` or
   * `ShadowRoot` and adopts the stylesheet once per root, scoping `:host` and descendant
   * selectors to the element's `tagName`.
   *
   * Does nothing when the element renders into a `ShadowRoot` (the default for
   * `A2uiLitElement`), because Lit already adopts `static styles` into that `ShadowRoot`.
   */
  adoptLightDomStyles() {
    if (typeof document === "undefined")
      return;
    if (this.renderRoot !== this)
      return;
    const root = this.getRootNode();
    const constructor = this.constructor;
    const styles = constructor.styles;
    if (!styles)
      return;
    const tagName = this.tagName.toLowerCase();
    if (!constructor._processedSheet && constructor._processedCss === void 0) {
      const styleList = Array.isArray(styles) ? styles : [styles];
      const rawCss = styleList.map((s5) => s5 && typeof s5 === "object" && "cssText" in s5 ? String(s5.cssText) : String(s5)).join("\n");
      const baseCss = rawCss.replace(/:where\(:host\)/g, `:where(${tagName})`).replace(/:host\(([^)]+)\)/g, `${tagName}$1`).replace(/:host/g, tagName);
      let processedCss = baseCss;
      try {
        const sheet = new CSSStyleSheet();
        sheet.replaceSync(baseCss);
        const scopeRule = (rule) => {
          if (typeof CSSStyleRule !== "undefined" && rule instanceof CSSStyleRule) {
            const scopedSelectors = rule.selectorText.split(",").map((sel) => {
              sel = sel.trim();
              if (sel === tagName || sel.startsWith(tagName + " ") || sel.startsWith(tagName + ".") || sel.startsWith(tagName + ":") || sel.startsWith(tagName + "[") || sel.startsWith(`:where(${tagName}`) || sel.startsWith(`:is(${tagName}`)) {
                return sel;
              }
              return `${tagName} ${sel}`;
            }).join(", ");
            return `${scopedSelectors} { ${rule.style.cssText} }`;
          } else if (typeof CSSMediaRule !== "undefined" && rule instanceof CSSMediaRule) {
            const inner = Array.from(rule.cssRules).map(scopeRule).join("\n");
            return `@media ${rule.conditionText} {
${inner}
}`;
          }
          return rule.cssText;
        };
        processedCss = Array.from(sheet.cssRules).map(scopeRule).join("\n");
        const scopedSheet = new CSSStyleSheet();
        scopedSheet.replaceSync(processedCss);
        constructor._processedSheet = scopedSheet;
      } catch {
      }
      constructor._processedCss = processedCss;
      constructor._processedStyle = i2([processedCss]);
      constructor._adoptedRoots = /* @__PURE__ */ new WeakSet();
    }
    const target = typeof ShadowRoot !== "undefined" && root instanceof ShadowRoot ? root : typeof document !== "undefined" ? document : void 0;
    if (target) {
      if (!constructor._adoptedRoots) {
        constructor._adoptedRoots = /* @__PURE__ */ new WeakSet();
      }
      if (!constructor._adoptedRoots.has(target)) {
        constructor._adoptedRoots.add(target);
        if (constructor._processedSheet && target.adoptedStyleSheets) {
          target.adoptedStyleSheets = [
            ...target.adoptedStyleSheets,
            constructor._processedSheet
          ];
        }
      }
    }
  }
  /**
   * Lifecycle hook invoked when the element is connected to the DOM.
   * Scopes and adopts component styles into the document or host shadow root.
   */
  connectedCallback() {
    super.connectedCallback();
    this.adoptLightDomStyles();
  }
  /**
   * Instantiates the controller for this element's specific bound API.
   *
   * Uses the instance `api` property when a subclass defines it. Otherwise it
   * uses the API of the catalog entry the component was rendered from (see
   * `findCatalogApi`). Subclasses can override this method if custom
   * controller initialization is required.
   *
   * @returns A new instance of `A2uiController` matching the component API.
   */
  createController() {
    const activeApi = this.api ?? this.findCatalogApi();
    if (!activeApi) {
      throw new Error(`[A2uiLitElement] Either define 'protected readonly api = ...' on ${this.constructor.name} or override 'createController()'.`);
    }
    return new A2uiController(this, activeApi);
  }
  /**
   * Finds the API of the catalog entry this element was rendered from.
   *
   * Version-agnostic elements, such as the basic catalog's, leave `api` unset:
   * one element class is registered in the catalogs of several protocol
   * versions, and the props schema has to match the catalog the component
   * came from. That entry is the resolved node's implementation, or the
   * component's entry in its own catalog.
   */
  findCatalogApi() {
    if (this.node?.impl) {
      return this.node.impl;
    }
    const componentModel = this.context?.componentModel;
    if (!componentModel)
      return void 0;
    const catalog = componentModel.catalog;
    return catalog.components.get(componentModel.type);
  }
  /**
   * Helper method to render a child A2UI node.
   * Abstracts away the need to manually create a ComponentContext.
   *
   * @param childRef The reference to the child component to render. Either a string ID
   *                 or a reference object containing `{id, basePath}`.
   * @param customPath An explicit data model path to bind the child to. If provided,
   *                   this overrides any path defined in the `childRef` object. If omitted,
   *                   falls back to the `childRef`'s `basePath`, or the current component's path.
   *
   * @returns A Lit template result containing the rendered child component, or `nothing` if the reference is empty.
   */
  renderNode(childRef, customPath) {
    if (!childRef || !this.context?.dataContext)
      return A;
    const { surface, path: parentPath } = this.context.dataContext;
    if (!surface) {
      return A;
    }
    const surfaceContainsComponent = !!surface.componentsModel?.get(this.context.componentModel.id);
    if (!surfaceContainsComponent) {
      return A;
    }
    if (isComponentNode(childRef) && !customPath) {
      return renderA2uiNode(childRef);
    }
    let componentId;
    let path = customPath;
    if (typeof childRef === "object") {
      componentId = childRef.id || childRef.componentId;
      path = path ?? childRef.basePath ?? childRef.path;
    } else {
      componentId = childRef;
    }
    if (!componentId || !surface.componentsModel?.get(componentId)) {
      return A;
    }
    path = path ?? parentPath;
    if (this.node) {
      const childNode = findChildNode(this.node, componentId, path);
      if (childNode) {
        return renderA2uiNode(childNode);
      }
    }
    const childContext = new ComponentContext(surface, componentId, path);
    const childCatalog = childContext.componentModel.catalog;
    return renderA2uiNode(childContext, childCatalog);
  }
  /**
   * Reacts to changes in the component's properties.
   *
   * Specifically, when the `node` or `context` property changes or is initialized,
   * this method cleans up any existing controller and invokes `createController()`
   * to bind to the new node or context.
   *
   * @param changedProperties Map of changed properties with their previous values.
   */
  willUpdate(changedProperties) {
    super.willUpdate(changedProperties);
    const nodeChanged = changedProperties.has("node") && this.node !== this._controllerNode;
    let contextChanged = changedProperties.has("context");
    if (changedProperties.has("node") && this.node?.context && this.node.context !== this.context) {
      this.context = this.node.context;
      contextChanged = true;
    }
    if ((contextChanged || nodeChanged) && this.context) {
      if (this._controller && !nodeChanged && bindsSameComponent(this._controllerContext, this.context)) {
        this._controllerContext = this.context;
        return;
      }
      if (this._controller) {
        this.removeController(this._controller);
        this._controller.dispose();
      }
      this._controller = this.createController();
      this._controllerContext = this.context;
      this._controllerNode = this.node;
    }
  }
  update(changedProperties) {
    if (!this._controller) {
      return;
    }
    super.update(changedProperties);
  }
};
__decorate([
  n5({ type: Object })
], A2uiLitElement.prototype, "context", void 0);
__decorate([
  n5({ type: Object })
], A2uiLitElement.prototype, "node", void 0);
function bindsSameComponent(a4, b4) {
  if (a4 === b4)
    return true;
  if (!a4?.componentModel || !b4?.componentModel || !a4.dataContext || !b4.dataContext) {
    return false;
  }
  return a4.componentModel === b4.componentModel && a4.surfaceComponents === b4.surfaceComponents && a4.theme === b4.theme && a4.dataContext.surface === b4.dataContext.surface && a4.dataContext.dataModel === b4.dataContext.dataModel && a4.dataContext.path === b4.dataContext.path && a4.dataContext.explicitIndex === b4.dataContext.explicitIndex && a4.dataContext.parent === b4.dataContext.parent;
}
function findChildNode(parent, componentId, dataPath) {
  const visit = (value) => {
    if (isComponentNode(value)) {
      return value.componentId === componentId && value.dataPath === dataPath ? value : void 0;
    }
    if (value instanceof ResolvedBinding) {
      return visit(value.value);
    }
    if (Array.isArray(value)) {
      for (const item of value) {
        const found = visit(item);
        if (found)
          return found;
      }
      return void 0;
    }
    if (value && typeof value === "object" && Object.getPrototypeOf(value) === Object.prototype) {
      for (const item of Object.values(value)) {
        const found = visit(item);
        if (found)
          return found;
      }
    }
    return void 0;
  };
  return visit(peekValue(parent.props));
}

// node_modules/@a2ui/web_core/src/resolution/ref-fields.js
var COMPONENT_ID_REF = "REF:common_types.json#/$defs/ComponentId";
var CHILD_LIST_REF = "REF:common_types.json#/$defs/ChildList";
var EMPTY_REF_FIELDS = /* @__PURE__ */ new Map();
var refFieldsCache = /* @__PURE__ */ new WeakMap();
function extractRefFields(schema) {
  const cached = refFieldsCache.get(schema);
  if (cached) {
    return cached;
  }
  const unwrapped = unwrap2(schema);
  if (unwrapped.schema._def.typeName !== "ZodObject") {
    refFieldsCache.set(schema, EMPTY_REF_FIELDS);
    return EMPTY_REF_FIELDS;
  }
  const fields = /* @__PURE__ */ new Map();
  const shape = unwrapped.schema.shape;
  for (const [key, value] of Object.entries(shape)) {
    const field = unwrap2(value);
    if (marksChildList(field) || isChildListUnion(field.schema)) {
      fields.set(key, { kind: "list" });
      continue;
    }
    if (marksComponentId(field)) {
      fields.set(key, { kind: "single" });
      continue;
    }
    if (field.schema._def.typeName === "ZodArray") {
      const element = unwrap2(field.schema._def.type);
      if (marksComponentId(element)) {
        fields.set(key, { kind: "list" });
        continue;
      }
      if (element.schema._def.typeName === "ZodObject") {
        const subKeys = /* @__PURE__ */ new Set();
        const elementShape = element.schema.shape;
        for (const [subKey, subValue] of Object.entries(elementShape)) {
          if (marksComponentId(unwrap2(subValue))) {
            subKeys.add(subKey);
          }
        }
        if (subKeys.size > 0) {
          fields.set(key, { kind: "nested", keys: subKeys });
        }
      }
    }
  }
  const result = fields;
  refFieldsCache.set(schema, result);
  return result;
}
function unwrap2(schema) {
  const descriptions = [];
  const refs = /* @__PURE__ */ new Set();
  let current = schema;
  for (; ; ) {
    if (current.description) {
      descriptions.push(current.description);
    }
    const ref = childRefKindOf(current);
    if (ref) {
      refs.add(ref);
    }
    const typeName = current._def.typeName;
    if (typeName === "ZodOptional" || typeName === "ZodNullable" || typeName === "ZodDefault") {
      current = current._def.innerType;
    } else if (typeName === "ZodEffects") {
      current = current._def.schema;
    } else {
      return { schema: current, descriptions, refs };
    }
  }
}
function marksComponentId(field) {
  return field.refs.has("component-id") || hasPointer(field.descriptions, COMPONENT_ID_REF);
}
function marksChildList(field) {
  return field.refs.has("child-list") || hasPointer(field.descriptions, CHILD_LIST_REF);
}
function hasPointer(descriptions, pointer) {
  return descriptions.some((d4) => d4.startsWith(pointer));
}
function isChildListUnion(schema) {
  if (schema._def.typeName !== "ZodUnion") {
    return false;
  }
  const options = schema._def.options;
  return options.some((o7) => {
    if (o7._def.typeName !== "ZodObject") {
      return false;
    }
    const shape = o7.shape;
    return shape.componentId && shape.path;
  });
}

// node_modules/@a2ui/web_core/src/resolution/node-resolver.js
var ROOT_COMPONENT_ID = "root";
var ROOT_DATA_PATH = "/";
var EMPTY_REF_FIELDS2 = /* @__PURE__ */ new Map();
var NodeResolver = class {
  /**
   * Creates a new `NodeResolver` instance.
   *
   * Each component resolves against the catalog its model was bound to, so a
   * surface without a default catalog (allowed from v1.0) renders as long as
   * its components name their catalogs.
   *
   * @param surface Surface model to observe.
   * @param catalog The surface's default catalog. Defaults to
   *   `surface.defaultCatalog`, which may be `undefined`.
   * @throws {A2uiStateError} If the catalog instance differs from the surface's catalog.
   */
  constructor(surface, catalog = surface.defaultCatalog) {
    this.records = /* @__PURE__ */ new Map();
    this.nodesByEdge = /* @__PURE__ */ new Map();
    this.nodesByComponentId = /* @__PURE__ */ new Map();
    this.pendingParents = /* @__PURE__ */ new Map();
    this.dispatchedErrors = /* @__PURE__ */ new Map();
    this.modelSubs = [];
    this._disposed = false;
    if (catalog !== surface.defaultCatalog) {
      throw new A2uiStateError("NodeResolver requires the same catalog instance its surface was constructed with.");
    }
    this.surface = surface;
    this.rootId = surface.rootId ?? ROOT_COMPONENT_ID;
    this.rootNode = signal(void 0);
    if (surface.componentsModel?.onCreated) {
      this.modelSubs.push(surface.componentsModel.onCreated.subscribe((component) => this.onComponentCreated(component)));
    }
    if (surface.componentsModel?.onDeleted) {
      this.modelSubs.push(surface.componentsModel.onDeleted.subscribe((id) => this.onComponentDeleted(id)));
    }
    if (surface.componentsModel.get(this.rootId)) {
      this.buildRoot();
    }
  }
  /**
   * Dispatches once per (code, component, path) for as long as the condition
   * persists: the record is cleared when the component is deleted, when a
   * node for the pair resolves, or when a cyclic stand-in's edge goes away,
   * so a condition that is fixed and later reintroduced reports again.
   */
  dispatchOnce(code, componentId, dataPath, message) {
    let byPath = this.dispatchedErrors.get(componentId);
    if (!byPath) {
      byPath = /* @__PURE__ */ new Map();
      this.dispatchedErrors.set(componentId, byPath);
    }
    let codes = byPath.get(dataPath);
    if (!codes) {
      codes = /* @__PURE__ */ new Set();
      byPath.set(dataPath, codes);
    }
    if (codes.has(code)) {
      return;
    }
    codes.add(code);
    this.surface.dispatchError({ code, message });
  }
  clearDispatched(componentId, dataPath) {
    const byPath = this.dispatchedErrors.get(componentId);
    if (byPath?.delete(dataPath) && byPath.size === 0) {
      this.dispatchedErrors.delete(componentId);
    }
  }
  /** Total count of live nodes in the tree, including placeholders. */
  get activeNodeCount() {
    return this.records.size;
  }
  /** Whether this resolver has been disposed. */
  get disposed() {
    return this._disposed;
  }
  /**
   * Tears down the entire node tree and disconnects model event listeners.
   *
   * This operation is idempotent.
   */
  dispose() {
    if (this._disposed) {
      return;
    }
    this._disposed = true;
    for (const sub of this.modelSubs) {
      sub.unsubscribe();
    }
    this.modelSubs.length = 0;
    for (const node of [...this.records.keys()]) {
      this.disposeNode(node);
    }
    this.pendingParents.clear();
    this.dispatchedErrors.clear();
    this.rootRecord = void 0;
    setValue(this.rootNode, void 0);
  }
  /** No-op while a root record exists; root deletion clears it so a re-sent root rebuilds. */
  buildRoot() {
    if (this.rootRecord) {
      return;
    }
    const rootEdgeKey = `>${escapeIdPart(this.rootId)}>${escapeIdPart(this.rootId)}@/`;
    const node = this.createNode(this.rootId, ROOT_DATA_PATH, rootEdgeKey, void 0);
    this.rootRecord = this.records.get(node);
    setValue(this.rootNode, node);
  }
  onComponentCreated(component) {
    if (this._disposed) {
      return;
    }
    if (component.id === this.rootId) {
      const current = this.surface.componentsModel.get(this.rootId);
      if (this.rootRecord && this.rootRecord.componentModel !== current) {
        this.disposeNode(this.rootRecord.node);
        this.rootRecord = void 0;
        if (!current) {
          setValue(this.rootNode, void 0);
        }
      }
      if (current) {
        this.buildRoot();
      }
    }
    const waiting = this.pendingParents.get(component.id);
    if (waiting) {
      this.pendingParents.delete(component.id);
      for (const parent of waiting) {
        const record = this.records.get(parent);
        if (record && !parent.disposed) {
          this.materialize(record);
        }
      }
    }
  }
  onComponentDeleted(id) {
    if (this._disposed) {
      return;
    }
    this.dispatchedErrors.delete(id);
    const model = this.surface.componentsModel.get(id);
    const affected = this.nodesByComponentId.get(id);
    if (!affected) {
      return;
    }
    const parentsToRefresh = /* @__PURE__ */ new Set();
    let rootAffected = false;
    for (const node of [...affected]) {
      const record = this.records.get(node);
      if (!record) {
        continue;
      }
      if (record.parent) {
        parentsToRefresh.add(record.parent);
      } else {
        rootAffected = true;
      }
    }
    if (rootAffected && this.rootRecord) {
      if (!model) {
        this.disposeNode(this.rootRecord.node);
        this.rootRecord = void 0;
        setValue(this.rootNode, void 0);
      } else if (this.rootRecord.componentModel !== model) {
        this.disposeNode(this.rootRecord.node);
        this.rootRecord = void 0;
        this.buildRoot();
      }
    }
    for (const parent of parentsToRefresh) {
      const record = this.records.get(parent);
      if (record && !parent.disposed) {
        this.materialize(record);
      }
    }
  }
  /**
   * Creates a node for one (componentId, dataPath) edge. A missing component
   * definition yields a placeholder node and registers the parent for a
   * refresh when the definition arrives.
   */
  createNode(componentId, dataPath, edgeKey, parent, occurrence = 1) {
    const model = this.surface.componentsModel.get(componentId);
    if (!model) {
      const record2 = this.registerNode(new MutableComponentNode(instanceIdFor(componentId, dataPath, occurrence), componentId, PLACEHOLDER_TYPE, dataPath, {}, void 0, void 0, "pending"), { edgeKey, parent, occurrence, refFields: EMPTY_REF_FIELDS2 });
      if (parent) {
        this.registerPendingParent(componentId, parent);
      }
      return record2.node;
    }
    const compCatalog = this.catalogFor(model);
    const api = compCatalog.components.get(model.type);
    if (!api) {
      this.dispatchOnce("UNKNOWN_COMPONENT_TYPE", componentId, dataPath, `Component '${componentId}' has type '${model.type}', which is not in catalog '${compCatalog.id}'.`);
      return this.registerNode(new MutableComponentNode(instanceIdFor(componentId, dataPath, occurrence), componentId, model.type, dataPath, {}, void 0, void 0, "unknown-type", computed(() => getValue(model.metadataSignal))), { edgeKey, parent, occurrence, refFields: EMPTY_REF_FIELDS2, componentModel: model }).node;
    }
    this.clearDispatched(componentId, dataPath);
    const context = new ComponentContext(this.surface, componentId, dataPath, parent?.context?.dataContext);
    const binder = new GenericBinder(context, api.schema);
    const record = this.registerNode(new MutableComponentNode(instanceIdFor(componentId, dataPath, occurrence), componentId, model.type, dataPath, {}, api, context, "resolved", computed(() => getValue(model.metadataSignal))), {
      edgeKey,
      parent,
      occurrence,
      refFields: extractRefFields(api.schema),
      behavior: scrapeSchemaBehavior(api.schema),
      componentModel: model,
      binder
    });
    record.binderSub = binder.subscribe((raw) => {
      record.lastBinderProps = raw;
      this.materialize(record);
    });
    record.lastBinderProps = binder.snapshot;
    this.materialize(record);
    return record.node;
  }
  /**
   * Returns the catalog a component model resolves against: the catalog it
   * was bound to, never the surface's default catalog directly.
   *
   * @param model Component model to resolve.
   * @returns The component model's catalog.
   */
  catalogFor(model) {
    return model.catalog;
  }
  registerNode(node, partial) {
    const record = {
      node,
      edgeKey: partial.edgeKey,
      parent: partial.parent,
      occurrence: partial.occurrence,
      refFields: partial.refFields,
      behavior: partial.behavior,
      componentModel: partial.componentModel,
      binder: partial.binder,
      childEdges: /* @__PURE__ */ new Map()
    };
    this.records.set(node, record);
    this.nodesByEdge.set(partial.edgeKey, node);
    let byId = this.nodesByComponentId.get(node.componentId);
    if (!byId) {
      byId = /* @__PURE__ */ new Set();
      this.nodesByComponentId.set(node.componentId, byId);
    }
    byId.add(node);
    return record;
  }
  /**
   * Returns the node for a child edge, reusing the cached node when the edge
   * is unchanged and replacing it (placeholder upgrade or downgrade, id
   * change, type change) when it is not.
   */
  childNode(componentId, dataPath, edgeKey, parent, occurrence = 1) {
    const existing = this.nodesByEdge.get(edgeKey);
    if (this.isCyclic(componentId, dataPath, parent)) {
      if (existing && !existing.disposed && existing.isPlaceholder && this.records.get(existing)?.occurrence === occurrence) {
        return existing;
      }
      if (existing && !existing.disposed) {
        this.disposeNode(existing);
      }
      this.dispatchOnce("CYCLIC_REFERENCE", componentId, dataPath, `Component '${componentId}' at '${dataPath}' is referenced by one of its own descendants; rendering a placeholder instead.`);
      return this.registerNode(new MutableComponentNode(instanceIdFor(componentId, dataPath, occurrence), componentId, PLACEHOLDER_TYPE, dataPath, {}, void 0, void 0, "cyclic"), { edgeKey, parent, occurrence, refFields: EMPTY_REF_FIELDS2 }).node;
    }
    if (existing && !existing.disposed) {
      const model = this.surface.componentsModel.get(componentId);
      const api = model ? this.catalogFor(model).components.get(model.type) : void 0;
      const existingRecord = this.records.get(existing);
      const upToDate = existing.componentId === componentId && existing.dataPath === dataPath && existingRecord?.occurrence === occurrence && (existing.isPlaceholder ? model === void 0 && existing.state === "pending" || model !== void 0 && api === void 0 && existing.state === "unknown-type" && existingRecord?.componentModel === model : model !== void 0 && existing.type === model.type && existingRecord?.componentModel === model);
      if (upToDate) {
        if (existing.state === "pending") {
          this.registerPendingParent(componentId, parent);
        }
        return existing;
      }
      this.disposeNode(existing);
    }
    return this.createNode(componentId, dataPath, edgeKey, parent, occurrence);
  }
  /** Registers a parent to be refreshed when `componentId` arrives. */
  registerPendingParent(componentId, parent) {
    let waiting = this.pendingParents.get(componentId);
    if (!waiting) {
      waiting = /* @__PURE__ */ new Set();
      this.pendingParents.set(componentId, waiting);
    }
    waiting.add(parent);
  }
  /** True when (componentId, dataPath) already appears in the parent chain. */
  isCyclic(componentId, dataPath, parent) {
    for (let node = parent; node; node = this.records.get(node)?.parent) {
      if (node.componentId === componentId && node.dataPath === dataPath) {
        return true;
      }
    }
    return false;
  }
  /**
   * Rebuilds a node's resolved props from its binder output: child reference
   * properties become live `ComponentNode`s, children this parent no longer
   * references are disposed, and unchanged values keep reference identity so
   * the shallow comparison in `MutableComponentNode.setProps` stays exact.
   */
  materialize(record) {
    if (record.node.disposed) {
      return;
    }
    const raw = record.lastBinderProps ?? record.binder?.snapshot ?? {};
    const next = { ...raw };
    const newEdges = /* @__PURE__ */ new Map();
    const modelProps = record.componentModel?.properties;
    if (modelProps) {
      for (const key of record.refFields.keys()) {
        if (key in next && !(key in modelProps)) {
          delete next[key];
        }
      }
    }
    const occurrences = /* @__PURE__ */ new Map();
    const resolveChild = (slot, componentId, dataPath) => {
      const occurrenceKey = `${escapeIdPart(componentId)}@${escapeIdPart(dataPath)}`;
      const occurrence = (occurrences.get(occurrenceKey) ?? 0) + 1;
      occurrences.set(occurrenceKey, occurrence);
      const edgeKey = `${record.edgeKey}>${slot}>${occurrenceKey}#${occurrence}`;
      const child = this.childNode(componentId, dataPath, edgeKey, record.node, occurrence);
      newEdges.set(edgeKey, child);
      return child;
    };
    for (const [key, ref] of record.refFields) {
      switch (ref.kind) {
        case "single": {
          const value = next[key];
          if (typeof value === "string" && value) {
            next[key] = resolveChild(escapeIdPart(key), value, record.node.dataPath);
          }
          break;
        }
        case "list": {
          const value = next[key];
          if (!Array.isArray(value)) {
            break;
          }
          const safeValue = getSafeChildList(value);
          const slotKey = escapeIdPart(key);
          next[key] = safeValue.map((item) => {
            if (typeof item === "string" && item) {
              return resolveChild(slotKey, item, record.node.dataPath);
            }
            if (item && typeof item === "object" && !Array.isArray(item)) {
              const entry = item;
              if (typeof entry.id === "string" && typeof entry.basePath === "string") {
                return resolveChild(slotKey, entry.id, entry.basePath);
              }
              if (typeof entry.componentId === "string" && entry.componentId) {
                return resolveChild(slotKey, entry.componentId, record.node.dataPath);
              }
            }
            return item;
          });
          break;
        }
        case "nested": {
          const value = next[key];
          if (!Array.isArray(value)) {
            break;
          }
          next[key] = value.map((item, index) => {
            if (!item || typeof item !== "object" || Array.isArray(item)) {
              return item;
            }
            const entry = { ...item };
            let resolvedAny = false;
            for (const subKey of ref.keys) {
              const childId = entry[subKey];
              if (typeof childId === "string" && childId) {
                entry[subKey] = resolveChild(`${escapeIdPart(key)}[${index}].${escapeIdPart(subKey)}`, childId, record.node.dataPath);
                resolvedAny = true;
              }
            }
            return resolvedAny ? entry : item;
          });
          break;
        }
      }
    }
    for (const [edgeKey, child] of record.childEdges) {
      if (!newEdges.has(edgeKey)) {
        this.disposeNode(child);
        if (child.isPlaceholder) {
          const stillWaiting = [...newEdges.values()].some((other) => other.isPlaceholder && other.componentId === child.componentId);
          if (!stillWaiting) {
            const waiting = this.pendingParents.get(child.componentId);
            if (waiting) {
              waiting.delete(record.node);
              if (waiting.size === 0) {
                this.pendingParents.delete(child.componentId);
              }
            }
          }
        }
      }
    }
    record.childEdges = newEdges;
    const wrapped = record.behavior && record.node.context ? wrapDynamicValues(next, record.behavior, modelProps ?? {}, record.node.context.dataContext) : next;
    const previous = peekValue(record.node.props);
    for (const key of Object.keys(wrapped)) {
      wrapped[key] = stabilize(previous[key], wrapped[key]);
    }
    record.node.setProps(wrapped);
  }
  /** Disposes a node and, through parent-scoped ownership, its subtree. */
  disposeNode(node) {
    if (node.disposed) {
      return;
    }
    const record = this.records.get(node);
    if (record) {
      for (const child of record.childEdges.values()) {
        this.disposeNode(child);
      }
      record.childEdges.clear();
      record.binderSub?.unsubscribe();
      record.binderSub = void 0;
      if (this.nodesByEdge.get(record.edgeKey) === node) {
        this.nodesByEdge.delete(record.edgeKey);
      }
      this.records.delete(node);
    }
    const byId = this.nodesByComponentId.get(node.componentId);
    if (byId) {
      byId.delete(node);
      if (byId.size === 0) {
        this.nodesByComponentId.delete(node.componentId);
      }
    }
    for (const [componentId, waiting] of this.pendingParents) {
      waiting.delete(node);
      if (waiting.size === 0) {
        this.pendingParents.delete(componentId);
      }
    }
    if (node.state === "cyclic") {
      this.clearDispatched(node.componentId, node.dataPath);
    }
    node.dispose();
  }
};
var ID_PART_ESCAPES = {
  "~": "~0",
  "#": "~1",
  "[": "~2",
  "]": "~3",
  ">": "~4",
  "@": "~5"
};
function escapeIdPart(part) {
  return part.replace(/[~#[\]>@]/g, (match) => ID_PART_ESCAPES[match]);
}
function instanceIdFor(componentId, dataPath, occurrence) {
  const trimmed = dataPath.replace(/\/+$/, "") || ROOT_DATA_PATH;
  const base = dataPath === ROOT_DATA_PATH ? escapeIdPart(componentId) : `${escapeIdPart(componentId)}-[${escapeIdPart(trimmed)}]`;
  return occurrence > 1 ? `${base}#${occurrence}` : base;
}
function wrapDynamicValues(value, behavior, raw, dataContext) {
  switch (behavior.type) {
    case "DYNAMIC": {
      const bindingKey = dataContext.atLeastV10 ? "@path" : "path";
      const rawRecord = raw && typeof raw === "object" && !Array.isArray(raw) ? raw : void 0;
      const path = rawRecord && typeof rawRecord[bindingKey] === "string" && !("componentId" in rawRecord) ? rawRecord[bindingKey] : void 0;
      if (path === void 0) {
        return new ResolvedBinding(value);
      }
      return new WritableBinding(value, (newValue) => dataContext.set(path, newValue), path);
    }
    case "ARRAY": {
      if (!Array.isArray(value)) {
        return value;
      }
      const rawItems = Array.isArray(raw) ? raw : [];
      return value.map((item, index) => wrapDynamicValues(item, behavior.element, rawItems[index], dataContext));
    }
    case "OBJECT": {
      if (!value || typeof value !== "object" || Array.isArray(value)) {
        return value;
      }
      const setterNames = /* @__PURE__ */ new Set();
      for (const [key, childBehavior] of Object.entries(behavior.shape)) {
        if (childBehavior.type === "DYNAMIC") {
          setterNames.add(`set${key.charAt(0).toUpperCase()}${key.slice(1)}`);
        }
      }
      const rawRecord = raw && typeof raw === "object" && !Array.isArray(raw) ? raw : {};
      const out = {};
      for (const [key, item] of Object.entries(value)) {
        if (setterNames.has(key)) {
          continue;
        }
        out[key] = wrapDynamicValues(item, behavior.shape[key] ?? { type: "STATIC" }, rawRecord[key], dataContext);
      }
      for (const [key, childBehavior] of Object.entries(behavior.shape)) {
        if (childBehavior.type === "DYNAMIC" && !(key in out)) {
          out[key] = new ResolvedBinding(void 0);
        }
      }
      return out;
    }
    default:
      return value;
  }
}
function stabilize(prev, next) {
  if (Object.is(prev, next)) {
    return next;
  }
  if (prev instanceof ResolvedBinding && next instanceof ResolvedBinding) {
    return sameBinding(prev, next) ? prev : next;
  }
  if (isComponentNode(prev) || isComponentNode(next)) {
    return next;
  }
  if (Array.isArray(prev) && Array.isArray(next) && prev.length === next.length) {
    let allSame = true;
    const out = next.map((item, i7) => {
      const stabilized = stabilize(prev[i7], item);
      if (!Object.is(stabilized, prev[i7])) {
        allSame = false;
      }
      return stabilized;
    });
    return allSame ? prev : out;
  }
  if (isPlainObject7(prev) && isPlainObject7(next)) {
    const prevKeys = Object.keys(prev);
    const nextKeys = Object.keys(next);
    if (prevKeys.length === nextKeys.length) {
      let allSame = true;
      const out = {};
      for (const key of nextKeys) {
        const stabilized = stabilize(prev[key], next[key]);
        out[key] = stabilized;
        if (!(key in prev) || !Object.is(stabilized, prev[key])) {
          allSame = false;
        }
      }
      return allSame ? prev : out;
    }
  }
  return next;
}
function isPlainObject7(value) {
  if (!value || typeof value !== "object" || Array.isArray(value) || isComponentNode(value)) {
    return false;
  }
  const proto = Object.getPrototypeOf(value);
  return proto === Object.prototype || proto === null;
}

// node_modules/@a2ui/lit/src/surface/a2ui-surface.js
var __esDecorate = function(ctor, descriptorIn, decorators, contextIn, initializers, extraInitializers) {
  function accept(f4) {
    if (f4 !== void 0 && typeof f4 !== "function") throw new TypeError("Function expected");
    return f4;
  }
  var kind = contextIn.kind, key = kind === "getter" ? "get" : kind === "setter" ? "set" : "value";
  var target = !descriptorIn && ctor ? contextIn["static"] ? ctor : ctor.prototype : null;
  var descriptor = descriptorIn || (target ? Object.getOwnPropertyDescriptor(target, contextIn.name) : {});
  var _3, done = false;
  for (var i7 = decorators.length - 1; i7 >= 0; i7--) {
    var context = {};
    for (var p5 in contextIn) context[p5] = p5 === "access" ? {} : contextIn[p5];
    for (var p5 in contextIn.access) context.access[p5] = contextIn.access[p5];
    context.addInitializer = function(f4) {
      if (done) throw new TypeError("Cannot add initializers after decoration has completed");
      extraInitializers.push(accept(f4 || null));
    };
    var result = (0, decorators[i7])(kind === "accessor" ? { get: descriptor.get, set: descriptor.set } : descriptor[key], context);
    if (kind === "accessor") {
      if (result === void 0) continue;
      if (result === null || typeof result !== "object") throw new TypeError("Object expected");
      if (_3 = accept(result.get)) descriptor.get = _3;
      if (_3 = accept(result.set)) descriptor.set = _3;
      if (_3 = accept(result.init)) initializers.unshift(_3);
    } else if (_3 = accept(result)) {
      if (kind === "field") initializers.unshift(_3);
      else descriptor[key] = _3;
    }
  }
  if (target) Object.defineProperty(target, contextIn.name, descriptor);
  done = true;
};
var __runInitializers = function(thisArg, initializers, value) {
  var useValue = arguments.length > 2;
  for (var i7 = 0; i7 < initializers.length; i7++) {
    value = useValue ? initializers[i7].call(thisArg, value) : initializers[i7].call(thisArg);
  }
  return useValue ? value : void 0;
};
var A2uiSurface = (() => {
  let _classDecorators = [t4("a2ui-surface")];
  let _classDescriptor;
  let _classExtraInitializers = [];
  let _classThis;
  let _classSuper = i5;
  let _surface_decorators;
  let _surface_initializers = [];
  let _surface_extraInitializers = [];
  let __rootNode_decorators;
  let __rootNode_initializers = [];
  let __rootNode_extraInitializers = [];
  var A2uiSurface2 = class extends _classSuper {
    static {
      _classThis = this;
    }
    constructor() {
      super(...arguments);
      this.#surface_accessor_storage = __runInitializers(this, _surface_initializers, void 0);
      this.#_rootNode_accessor_storage = (__runInitializers(this, _surface_extraInitializers), __runInitializers(this, __rootNode_initializers, void 0));
      this.unsubscribe = __runInitializers(this, __rootNode_extraInitializers);
    }
    static {
      const _metadata = typeof Symbol === "function" && Symbol.metadata ? Object.create(_classSuper[Symbol.metadata] ?? null) : void 0;
      _surface_decorators = [n5({ type: Object })];
      __rootNode_decorators = [r6()];
      __esDecorate(this, null, _surface_decorators, { kind: "accessor", name: "surface", static: false, private: false, access: { has: (obj) => "surface" in obj, get: (obj) => obj.surface, set: (obj, value) => {
        obj.surface = value;
      } }, metadata: _metadata }, _surface_initializers, _surface_extraInitializers);
      __esDecorate(this, null, __rootNode_decorators, { kind: "accessor", name: "_rootNode", static: false, private: false, access: { has: (obj) => "_rootNode" in obj, get: (obj) => obj._rootNode, set: (obj, value) => {
        obj._rootNode = value;
      } }, metadata: _metadata }, __rootNode_initializers, __rootNode_extraInitializers);
      __esDecorate(null, _classDescriptor = { value: _classThis }, _classDecorators, { kind: "class", name: _classThis.name, metadata: _metadata }, null, _classExtraInitializers);
      A2uiSurface2 = _classThis = _classDescriptor.value;
      if (_metadata) Object.defineProperty(_classThis, Symbol.metadata, { enumerable: true, configurable: true, writable: true, value: _metadata });
      __runInitializers(_classThis, _classExtraInitializers);
    }
    #surface_accessor_storage;
    /**
     * The surface model containing the component tree and catalog.
     */
    get surface() {
      return this.#surface_accessor_storage;
    }
    set surface(value) {
      this.#surface_accessor_storage = value;
    }
    #_rootNode_accessor_storage;
    /**
     * Resolved root component node from `NodeResolver`.
     * @internal
     */
    get _rootNode() {
      return this.#_rootNode_accessor_storage;
    }
    set _rootNode(value) {
      this.#_rootNode_accessor_storage = value;
    }
    /**
     * Returns the currently resolved root component node for this surface, if any.
     */
    get rootNode() {
      return this._rootNode;
    }
    /**
     * Renders into the element's direct children (Light DOM) instead of a ShadowRoot
     * so host stylesheets can target components inside the surface.
     */
    createRenderRoot() {
      return this;
    }
    /**
     * Handles lifecycle updates, specifically when the `surface` property changes.
     *
     * It subscribes to the components model so the surface re-renders whenever
     * the 'root' component is created or deleted.
     *
     * @param changedProperties Map of changed properties.
     */
    willUpdate(changedProperties) {
      if (changedProperties.has("surface")) {
        this.subscribeToRoot();
      }
    }
    /**
     * Restores the root subscriptions when the element is reattached.
     */
    connectedCallback() {
      super.connectedCallback();
      if (this.surface && !this.unsubscribe) {
        this.subscribeToRoot();
      }
    }
    /**
     * Cleans up subscriptions.
     */
    disconnectedCallback() {
      super.disconnectedCallback();
      this.unsubscribe?.();
      this.unsubscribe = void 0;
      this._rootNode = void 0;
    }
    /**
     * Tracks the 'root' component of the current surface via `NodeResolver` for
     * as long as the surface is shown.
     *
     * The processor replaces a component's model when an update changes its
     * type or catalog, deleting the old model and creating a new one, so the
     * subscription outlives the first creation of the root: the surface then
     * renders the new model.
     */
    subscribeToRoot() {
      this.unsubscribe?.();
      this.unsubscribe = void 0;
      this._rootNode = void 0;
      if (!this.surface) {
        return;
      }
      const resolver = new NodeResolver(this.surface);
      const stopEffect = effect(() => {
        this._rootNode = getValue(resolver.rootNode);
      });
      this.unsubscribe = () => {
        stopEffect();
        resolver.dispose();
      };
    }
    /**
     * Renders the surface.
     *
     * If `surface` is not set, or the root has not resolved (it has not arrived,
     * has an unknown type, or is part of a cycle), returns `nothing`.
     * Otherwise, renders the root component using `renderA2uiNode`.
     */
    render() {
      if (!this.surface)
        return A;
      const root = this._rootNode;
      if (!root || root.isPlaceholder)
        return A;
      try {
        return renderA2uiNode(root);
      } catch (e7) {
        console.error("Error rendering root node:", e7);
        return b3`<div>Error rendering surface</div>`;
      }
    }
  };
  return A2uiSurface2 = _classThis;
})();

// node_modules/lit-html/directive-helpers.js
var { I: t6 } = j2;
var r7 = (o7) => void 0 === o7.strings;
var m3 = {};
var p4 = (o7, t7 = m3) => o7._$AH = t7;

// node_modules/lit-html/directives/live.js
var l4 = e6(class extends i6 {
  constructor(r8) {
    if (super(r8), r8.type !== t5.PROPERTY && r8.type !== t5.ATTRIBUTE && r8.type !== t5.BOOLEAN_ATTRIBUTE) throw Error("The `live` directive is not allowed on child or event bindings");
    if (!r7(r8)) throw Error("`live` bindings can only contain a single expression");
  }
  render(r8) {
    return r8;
  }
  update(i7, [t7]) {
    if (t7 === E || t7 === A) return t7;
    const o7 = i7.element, l5 = i7.name;
    if (i7.type === t5.PROPERTY) {
      if (t7 === o7[l5]) return E;
    } else if (i7.type === t5.BOOLEAN_ATTRIBUTE) {
      if (!!t7 === o7.hasAttribute(l5)) return E;
    } else if (i7.type === t5.ATTRIBUTE && o7.getAttribute(l5) === t7 + "") return E;
    return p4(i7), t7;
  }
});
export {
  A2uiLitElement,
  A2uiSurface,
  AudioPlayerApi,
  ButtonApi,
  CardApi,
  Catalog,
  CheckBoxApi,
  ChoicePickerApi,
  ColumnApi,
  ComponentContext,
  DateTimeInputApi,
  DividerApi,
  GenericBinder,
  IconApi,
  ImageApi,
  ListApi,
  MessageProcessor,
  ModalApi,
  RowApi,
  SliderApi,
  TabsApi,
  TextApi,
  TextFieldApi,
  V09_SPEC_FUNCTIONS,
  VideoApi,
  createFunctionImplementation,
  b3 as html,
  l4 as live,
  A as nothing
};
/*! Bundled license information:

@a2ui/web_core/src/catalog/exclusive_union.js:
@a2ui/web_core/src/catalog/schema_serializer.js:
@a2ui/web_core/src/catalog/schema_generator.js:
  (*
   * @license
   * Copyright 2024 Google LLC
   *
   * Licensed under the Apache License, Version 2.0 (the "License");
   * you may not use this file except in compliance with the License.
   * You may obtain a copy of the License at
   *
   *   https://www.apache.org/licenses/LICENSE-2.0
   *
   * Unless required by applicable law or agreed to in writing, software
   * distributed under the License is distributed on an "AS IS" BASIS,
   * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   * See the License for the specific language governing permissions and
   * limitations under the License.
   *)

@lit/reactive-element/css-tag.js:
  (**
   * @license
   * Copyright 2019 Google LLC
   * SPDX-License-Identifier: BSD-3-Clause
   *)

@lit/reactive-element/reactive-element.js:
lit-html/lit-html.js:
lit-element/lit-element.js:
@lit/reactive-element/decorators/custom-element.js:
@lit/reactive-element/decorators/property.js:
@lit/reactive-element/decorators/state.js:
@lit/reactive-element/decorators/event-options.js:
@lit/reactive-element/decorators/base.js:
@lit/reactive-element/decorators/query.js:
@lit/reactive-element/decorators/query-all.js:
@lit/reactive-element/decorators/query-async.js:
@lit/reactive-element/decorators/query-assigned-nodes.js:
lit-html/directive.js:
  (**
   * @license
   * Copyright 2017 Google LLC
   * SPDX-License-Identifier: BSD-3-Clause
   *)

lit-html/is-server.js:
  (**
   * @license
   * Copyright 2022 Google LLC
   * SPDX-License-Identifier: BSD-3-Clause
   *)

@lit/reactive-element/decorators/query-assigned-elements.js:
  (**
   * @license
   * Copyright 2021 Google LLC
   * SPDX-License-Identifier: BSD-3-Clause
   *)

lit-html/directive-helpers.js:
lit-html/directives/live.js:
  (**
   * @license
   * Copyright 2020 Google LLC
   * SPDX-License-Identifier: BSD-3-Clause
   *)
*/
