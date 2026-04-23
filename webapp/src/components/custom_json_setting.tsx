import React, { useState, ChangeEvent } from 'react';

// Структура одного объекта в массиве
interface WebhookItem {
  enabled?: boolean;
  display_name?: string;
  trigger_words?: string[];
  channel_ids?: string[];
  callback_urls?: string[];
  trigger_when?: string;
  content_type?: string;
  secret?: string;
  checkBotAccess?: boolean;
}

interface CustomJsonSettingProps {
  id: string;
  value?: string;
  onChange: (id: string, value: string) => void;
  disabled?: boolean;
}

const CustomJsonSetting: React.FC<CustomJsonSettingProps> = ({
  id,
  value = '[]', // Теперь по умолчанию пустой массив
  onChange,
  disabled = false,
}) => {
  const [textValue, setTextValue] = useState<string>(value);
  const [error, setError] = useState<string | null>(null);

  const validateAll = (data: any): string | null => {
    if (!Array.isArray(data)) {
      return "Root element must be an array [ ... ]";
    }

    for (let i = 0; i < data.length; i++) {
      const h = data[i] as WebhookItem;
      const prefix = `Item #${i + 1}: `;

      if (!h.display_name) {
        return `${prefix}display_name is required`;
      }
      if ((!h.trigger_words || h.trigger_words.length === 0) && 
          (!h.channel_ids || h.channel_ids.length === 0)) {
        return `${prefix}at least one trigger_word or channel_id is required`;
      }
      if (!h.callback_urls || h.callback_urls.length === 0) {
        return `${prefix}at least one callback_urls is required`;
      }
      
      const validTriggers = ["startswith", "exact", "regex"];
      if (h.trigger_when && !validTriggers.includes(h.trigger_when)) {
        return `${prefix}trigger_when must be one of: ${validTriggers.join(", ")}`;
      }

      const validContentTypes = ["application/json", "application/x-www-form-urlencoded"];
      if (h.content_type && !validContentTypes.includes(h.content_type)) {
        return `${prefix}content_type must be application/json or application/x-www-form-urlencoded`;
      }
    }

    return null;
  };

  const handleChange = (e: ChangeEvent<HTMLTextAreaElement>) => {
    const newValue = e.target.value;
    setTextValue(newValue);

    try {
      const parsed = JSON.parse(newValue);
      const validationError = validateAll(parsed);
      
      if (validationError) {
        setError(validationError);
      } else {
        setError(null);
        onChange(id, newValue);
      }
    } catch (err: any) {
      setError(`Invalid JSON: ${err.message}`);
    }
  };

  return (
    <div style={{ marginTop: '10px' }}>
      <textarea
        className="form-control"
        rows={15} // Увеличил высоту для массива
        value={textValue}
        onChange={handleChange}
        disabled={disabled}
        style={{ 
          fontFamily: 'monospace', 
          fontSize: '12px', 
          width: '100%',
          borderColor: error ? '#dc3545' : '#ced4da',
          padding: '10px'
        }}
      />
      {error && (
        <div style={{ color: '#dc3545', marginTop: '5px', fontSize: '12px', fontWeight: 'bold' }}>
          ✕ {error}
        </div>
      )}
    </div>
  );
};

export default CustomJsonSetting;