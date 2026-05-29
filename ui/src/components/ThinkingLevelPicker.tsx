import React, { useEffect, useRef, useState } from "react";

export type ThinkingLevel = "off" | "minimal" | "low" | "medium" | "high" | "xhigh";

export const DEFAULT_THINKING_LEVEL: ThinkingLevel = "medium";

export const THINKING_LEVELS: { value: ThinkingLevel; label: string }[] = [
  { value: "off", label: "off" },
  { value: "minimal", label: "minimal" },
  { value: "low", label: "low" },
  { value: "medium", label: "medium" },
  { value: "high", label: "high" },
  { value: "xhigh", label: "xhigh" },
];

interface ThinkingLevelPickerProps {
  value: ThinkingLevel;
  onChange: (level: ThinkingLevel) => void;
  disabled?: boolean;
}

function ThinkingLevelPicker({ value, onChange, disabled = false }: ThinkingLevelPickerProps) {
  const [isOpen, setIsOpen] = useState(false);
  const [openUpward, setOpenUpward] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!isOpen) return;
    function handleClickOutside(e: MouseEvent) {
      if (containerRef.current && !containerRef.current.contains(e.target as Node)) {
        setIsOpen(false);
      }
    }
    function handleKeyDown(e: KeyboardEvent) {
      if (e.key === "Escape") setIsOpen(false);
    }
    document.addEventListener("mousedown", handleClickOutside);
    document.addEventListener("keydown", handleKeyDown);
    return () => {
      document.removeEventListener("mousedown", handleClickOutside);
      document.removeEventListener("keydown", handleKeyDown);
    };
  }, [isOpen]);

  useEffect(() => {
    if (isOpen && containerRef.current) {
      const rect = containerRef.current.getBoundingClientRect();
      const spaceBelow = window.innerHeight - rect.bottom;
      const dropdownHeight = 260;
      setOpenUpward(spaceBelow < dropdownHeight && rect.top > spaceBelow);
    }
  }, [isOpen]);

  const current =
    THINKING_LEVELS.find((l) => l.value === value) ||
    THINKING_LEVELS.find((l) => l.value === DEFAULT_THINKING_LEVEL)!;

  return (
    <div className="model-picker thinking-level-picker" ref={containerRef}>
      <button
        className="model-picker-trigger"
        onClick={() => !disabled && setIsOpen(!isOpen)}
        disabled={disabled}
        type="button"
        title={`Reasoning effort: ${current.label}`}
      >
        <span className="model-picker-value">{current.label}</span>
        <svg
          className={`model-picker-chevron ${isOpen ? "open" : ""}`}
          width="12"
          height="12"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
        >
          <path d="M6 9l6 6 6-6" />
        </svg>
      </button>

      {isOpen && (
        <div className={`model-picker-dropdown ${openUpward ? "open-upward" : ""}`}>
          <div className="model-picker-options">
            {THINKING_LEVELS.map((level) => (
              <button
                key={level.value}
                className={`model-picker-option ${level.value === value ? "selected" : ""}`}
                onClick={() => {
                  onChange(level.value);
                  setIsOpen(false);
                }}
                type="button"
              >
                <div className="model-picker-option-content">
                  <span className="model-picker-option-name">{level.label}</span>
                </div>
                {level.value === value && (
                  <svg
                    className="model-picker-option-check"
                    width="14"
                    height="14"
                    viewBox="0 0 24 24"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="2"
                  >
                    <path d="M20 6L9 17l-5-5" />
                  </svg>
                )}
              </button>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

export default ThinkingLevelPicker;
