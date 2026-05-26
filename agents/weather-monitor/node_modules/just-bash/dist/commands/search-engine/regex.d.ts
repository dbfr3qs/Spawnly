/**
 * Regex building utilities for search commands
 */
import { type UserRegex } from "../../regex/index.js";
export type RegexMode = "basic" | "extended" | "fixed" | "perl";
export interface RegexOptions {
    mode: RegexMode;
    ignoreCase?: boolean;
    wholeWord?: boolean;
    lineRegexp?: boolean;
    multiline?: boolean;
    /** Makes . match newlines in multiline mode (ripgrep --multiline-dotall) */
    multilineDotall?: boolean;
}
export interface RegexResult {
    regex: UserRegex;
    /** If \K was used, this is the 1-based index of the capture group containing the "real" match */
    kResetGroup?: number;
    /**
     * Optional fast-path filter: if every needle is absent from a line via
     * String.indexOf, the regex is guaranteed not to match and RE2 can be skipped.
     * Extracted only for patterns where it is provably safe (literal alternatives,
     * optionally wrapped in \b...\b for -w mode). Null for anything more complex.
     */
    preFilter?: PreFilter;
}
export interface PreFilter {
    /** Any one of these substrings must appear in a matching line (OR semantics). */
    needles: string[];
    /** When true, both needles and the line must be lowercased before indexOf. */
    ignoreCase: boolean;
}
/**
 * Build a JavaScript RegExp from a pattern with the specified mode
 */
export declare function buildRegex(pattern: string, options: RegexOptions): RegexResult;
/**
 * Convert replacement string syntax to JavaScript's String.replace format
 *
 * Conversions:
 * - $0 and ${0} -> $& (full match)
 * - $name -> $<name> (named capture groups)
 * - ${name} -> $<name> (braced named capture groups)
 * - Preserves $1, $2, etc. for numbered groups
 */
export declare function convertReplacement(replacement: string): string;
